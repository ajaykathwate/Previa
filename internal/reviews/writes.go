package reviews

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/reviews/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/reviews"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Actor is who changes which pull request's review.
type Actor struct {
	UserID, RepoID int64
	Number         int32
}

// InstructionsInput is a save of the testing instructions. Revision is the
// one the form was opened at (0 when there were none); Overwrite saves over a
// newer revision on purpose after a conflict.
type InstructionsInput struct {
	Summary, Steps, Expected string
	StartPath, DocsPath      string
	Revision                 int64
}

// FindingInput starts a finding. Deployment is the deployment the page showed
// (0: none yet); AcknowledgeOlder records it although a newer one exists.
type FindingInput struct {
	Body, Path, Details string
	RequestKey          string
	Deployment          int32
	AcknowledgeOlder    bool
}

// ReplyInput adds a reply, written against the deployment the page showed.
type ReplyInput struct {
	Body, RequestKey string
	Deployment       int32
}

// EditInput replaces a message's text; Path and Details apply to a finding's
// opening message only.
type EditInput struct {
	Body, Path, Details string
	Revision            int64
}

// scope is one review write's locked context.
type scope struct {
	tx      pgx.Tx
	q       *dbgen.Queries
	actor   Actor
	pr      int64
	open    bool
	operate bool
	threads int32
	msgs    int32
	wrote   bool
}

// write runs fn in one transaction holding a review write's locks, in a fixed
// order: the pull request (key share), the writer's active membership (share)
// and the pull request's review counters. A write that changed something
// spends one unit of the writer's hourly allowance.
func (s *Service) write(ctx context.Context, a Actor, fn func(*scope) error) error {
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		pr, err := q.LockReviewPullRequest(ctx, dbgen.LockReviewPullRequestParams{RepoID: a.RepoID, Number: a.Number})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock the pull request: %w", err)
		}
		who, err := q.LockReviewer(ctx, dbgen.LockReviewerParams{UserID: a.UserID, RepoID: a.RepoID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !who.CanReview {
			return ErrUnavailable
		}
		if err != nil {
			return fmt.Errorf("lock the reviewer: %w", err)
		}
		if err := q.EnsureCounters(ctx, pr.ID); err != nil {
			return fmt.Errorf("prepare review counters: %w", err)
		}
		counters, err := q.LockCounters(ctx, pr.ID)
		if err != nil {
			return fmt.Errorf("lock review counters: %w", err)
		}
		sc := &scope{tx: tx, q: q, actor: a, pr: pr.ID, open: pr.State == "open", operate: who.CanOperate, threads: counters.Threads, msgs: counters.Messages}
		if err := fn(sc); err != nil {
			return err
		}
		if !sc.wrote {
			return nil
		}
		if _, err := q.SpendWriteBudget(ctx, dbgen.SpendWriteBudgetParams{UserID: a.UserID, Allowance: int32(s.opts.WriteAllowance)}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("count the review change: %w", err)
			}
			reset, err := q.WriteBudgetReset(ctx, a.UserID)
			if err != nil {
				return fmt.Errorf("read the review allowance: %w", err)
			}
			return &LimitError{ResetsAt: reset.Time}
		}
		return nil
	})
}

// record adds a timeline event in the write's transaction.
func (s *Service) record(ctx context.Context, sc *scope, e Event) error {
	if s.activity == nil {
		return nil
	}
	e.PullRequestID, e.ActorID = sc.pr, sc.actor.UserID
	return s.activity.RecordTx(ctx, sc.tx, e)
}

// SaveInstructions saves the testing instructions; only people who can build
// the pull request write them.
func (s *Service) SaveInstructions(ctx context.Context, a Actor, in InstructionsInput) error {
	fields := map[string]string{}
	var problem string
	if in.Summary, problem = cleanText(in.Summary, MaxSummaryRunes*4); problem != "" {
		fields["summary"] = problem
	} else if len([]rune(in.Summary)) > MaxSummaryRunes || strings.Contains(in.Summary, "\n") {
		fields["summary"] = "Keep the summary to one line of up to " + strconv.Itoa(MaxSummaryRunes) + " characters."
	}
	if in.Steps, problem = cleanText(in.Steps, MaxInstructionsBytes); problem != "" {
		fields["steps"] = problem
	} else if steps := (pages.Instructions{Steps: in.Steps}).StepList(); len(steps) > MaxSteps {
		fields["steps"] = "Use at most " + strconv.Itoa(MaxSteps) + " steps."
	} else {
		for _, step := range steps {
			if len([]rune(step)) > MaxStepRunes {
				fields["steps"] = "Keep each step under " + strconv.Itoa(MaxStepRunes) + " characters."
				break
			}
		}
	}
	if in.Expected, problem = cleanText(in.Expected, MaxInstructionsBytes); problem != "" {
		fields["expected"] = problem
	}
	if in.StartPath, problem = relativePath(in.StartPath); problem != "" {
		fields["start_path"] = problem
	}
	if in.DocsPath, problem = relativePath(in.DocsPath); problem != "" {
		fields["docs_path"] = problem
	}
	if len(fields) > 0 {
		return &InputError{Fields: fields, Message: "Check the highlighted fields."}
	}
	if len(in.Summary)+len(in.Steps)+len(in.Expected)+len(in.StartPath)+len(in.DocsPath) > MaxInstructionsBytes {
		return &InputError{Message: "Instructions are limited to 32 KB in total. Shorten them, then save."}
	}
	return s.write(ctx, a, func(sc *scope) error {
		if !sc.operate {
			return ErrUnavailable
		}
		if !sc.open {
			return ErrClosed
		}
		current, err := sc.q.LockInstructions(ctx, sc.pr)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("lock the testing instructions: %w", err)
		}
		if current.Revision != in.Revision {
			saved := pages.Instructions{Summary: current.Summary, Steps: current.Steps, Expected: current.Expected,
				StartPath: current.StartPath, DocsPath: current.DocsPath, Revision: current.Revision}
			if saved.Summary == in.Summary && saved.Steps == in.Steps && saved.Expected == in.Expected &&
				saved.StartPath == in.StartPath && saved.DocsPath == in.DocsPath {
				return nil // a repeated save of what is already there
			}
			return &ConflictError{Kind: "instructions", Saved: saved, Revision: current.Revision}
		}
		revision, err := sc.q.SaveInstructions(ctx, dbgen.SaveInstructionsParams{
			PullRequestID: sc.pr, Summary: in.Summary, Steps: in.Steps, Expected: in.Expected,
			StartPath: optionalText(in.StartPath), DocsPath: optionalText(in.DocsPath), UpdatedBy: a.UserID,
		})
		if err != nil {
			return fmt.Errorf("save the testing instructions: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindInstructionsUpdated, Metadata: map[string]any{"revision": revision},
			DedupKey: fmt.Sprintf("instructions:%d:%d", sc.pr, revision)})
	})
}

// CreateFinding starts a finding against the deployment the page showed. It
// returns the finding's number; a repeated request returns the first one's.
func (s *Service) CreateFinding(ctx context.Context, a Actor, in FindingInput) (int32, error) {
	fields := map[string]string{}
	var problem string
	if in.Body, problem = cleanText(in.Body, MaxMessageBytes); problem != "" {
		fields["body"] = problem
	} else if in.Body == "" {
		fields["body"] = "Describe what you found."
	}
	if in.Path, problem = relativePath(in.Path); problem != "" {
		fields["path"] = problem
	}
	if in.Details, problem = cleanText(in.Details, MaxMessageBytes); problem != "" {
		fields["details"] = problem
	}
	if len(fields) > 0 {
		return 0, &InputError{Fields: fields, Message: "Check the highlighted fields."}
	}
	if !validRequestKey(in.RequestKey) {
		return 0, &InputError{Message: "This form is out of date. Your text is kept: submit it again."}
	}
	var number int32
	err := s.write(ctx, a, func(sc *scope) error {
		if !sc.open {
			return ErrClosed
		}
		existing, err := sc.q.ThreadByKey(ctx, dbgen.ThreadByKeyParams{PullRequestID: sc.pr, IdempotencyKey: in.RequestKey})
		if err == nil {
			number = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check for a repeated finding: %w", err)
		}
		publication, err := s.recordedDeployment(ctx, sc, in.Deployment, in.AcknowledgeOlder)
		if err != nil {
			return err
		}
		if sc.threads >= MaxThreads {
			return &InputError{Message: "This pull request has reached its limit of " + strconv.Itoa(MaxThreads) + " findings. Reply to an existing finding instead."}
		}
		if sc.msgs >= MaxMessages {
			return &InputError{Message: "This pull request has reached its limit of " + strconv.Itoa(MaxMessages) + " messages."}
		}
		number = sc.threads + 1
		id, err := sc.q.CreateThread(ctx, dbgen.CreateThreadParams{
			PullRequestID: sc.pr, Number: number, PublicationID: optionalID(publication), AuthorUserID: a.UserID, IdempotencyKey: in.RequestKey,
		})
		if err != nil {
			return fmt.Errorf("save the finding: %w", err)
		}
		if _, err := sc.q.AddMessage(ctx, dbgen.AddMessageParams{
			ThreadID: id, PullRequestID: sc.pr, Position: 1, AuthorUserID: a.UserID, PublicationID: optionalID(publication),
			Body: in.Body, PagePath: optionalText(in.Path), Details: optionalText(in.Details),
		}); err != nil {
			return fmt.Errorf("save the finding's message: %w", err)
		}
		if err := sc.q.CountWrite(ctx, dbgen.CountWriteParams{PullRequestID: sc.pr, Threads: 1, Messages: 1}); err != nil {
			return fmt.Errorf("count the finding: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindFindingCreated, PublicationID: publication, Metadata: map[string]any{"finding": number},
			DedupKey: fmt.Sprintf("finding:%d:created", id)})
	})
	return number, err
}

// recordedDeployment is the publication a new finding is recorded against:
// the one the page showed, unless a newer one was published since and the
// writer has not chosen the older one on purpose.
func (s *Service) recordedDeployment(ctx context.Context, sc *scope, shown int32, older bool) (int64, error) {
	current, err := sc.q.CurrentPublication(ctx, sc.pr)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("read the current deployment: %w", err)
	}
	if shown == current.Sequence {
		return current.ID, nil
	}
	var tested *pages.Deployment
	var testedID int64
	if shown > 0 {
		row, err := sc.q.PublicationBySequence(ctx, dbgen.PublicationBySequenceParams{PullRequestID: sc.pr, Sequence: shown})
		if errors.Is(err, pgx.ErrNoRows) || shown > current.Sequence {
			return 0, &InputError{Message: "That deployment is unknown. Your text is kept: submit it again."}
		}
		if err != nil {
			return 0, fmt.Errorf("read the shown deployment: %w", err)
		}
		tested, testedID = &pages.Deployment{Sequence: row.Sequence, SHA: row.HeadSha}, row.ID
	}
	if older {
		return testedID, nil
	}
	return 0, &ConflictError{Kind: "version", Tested: tested, Current: deployment(current.Sequence, current.HeadSha)}
}

// Reply adds a reply to a finding and returns its position; a repeated
// request returns the first one's.
func (s *Service) Reply(ctx context.Context, a Actor, number int32, in ReplyInput) (int32, error) {
	var problem string
	if in.Body, problem = cleanText(in.Body, MaxMessageBytes); problem != "" {
		return 0, &InputError{Fields: map[string]string{"body": problem}}
	}
	if in.Body == "" {
		return 0, &InputError{Fields: map[string]string{"body": "Write a reply first."}}
	}
	if !validRequestKey(in.RequestKey) {
		return 0, &InputError{Message: "This form is out of date. Your text is kept: send it again."}
	}
	var position int32
	err := s.write(ctx, a, func(sc *scope) error {
		if !sc.open {
			return ErrClosed
		}
		t, err := lockThread(ctx, sc, number)
		if err != nil {
			return err
		}
		if t.State == pages.StateRemoved {
			return ErrGone
		}
		existing, err := sc.q.ReplyByKey(ctx, dbgen.ReplyByKeyParams{ThreadID: t.ID, IdempotencyKey: optionalText(in.RequestKey)})
		if err == nil {
			position = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check for a repeated reply: %w", err)
		}
		if t.Messages >= MaxThreadMessages {
			return &InputError{Message: "This finding has reached " + strconv.Itoa(MaxThreadMessages) + " messages. Start a new finding to continue."}
		}
		if sc.msgs >= MaxMessages {
			return &InputError{Message: "This pull request has reached its limit of " + strconv.Itoa(MaxMessages) + " messages."}
		}
		var publication int64
		if in.Deployment > 0 {
			row, err := sc.q.PublicationBySequence(ctx, dbgen.PublicationBySequenceParams{PullRequestID: sc.pr, Sequence: in.Deployment})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the shown deployment: %w", err)
			}
			publication = row.ID
		}
		position = t.Messages + 1
		id, err := sc.q.AddMessage(ctx, dbgen.AddMessageParams{
			ThreadID: t.ID, PullRequestID: sc.pr, Position: position, AuthorUserID: a.UserID, PublicationID: optionalID(publication),
			Body: in.Body, IdempotencyKey: optionalText(in.RequestKey),
		})
		if err != nil {
			return fmt.Errorf("save the reply: %w", err)
		}
		if err := sc.q.CountWrite(ctx, dbgen.CountWriteParams{PullRequestID: sc.pr, Messages: 1}); err != nil {
			return fmt.Errorf("count the reply: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindReplyAdded, PublicationID: publication, Metadata: map[string]any{"finding": number, "message": position},
			DedupKey: fmt.Sprintf("message:%d:created", id)})
	})
	return position, err
}

// EditMessage replaces the text of the writer's own message. It never changes
// the deployment the message or its finding were recorded against.
func (s *Service) EditMessage(ctx context.Context, a Actor, number, position int32, in EditInput) error {
	fields := map[string]string{}
	var problem string
	if in.Body, problem = cleanText(in.Body, MaxMessageBytes); problem != "" {
		fields["body"] = problem
	} else if in.Body == "" {
		fields["body"] = "A message cannot be empty. To take it back, remove it."
	}
	if in.Path, problem = relativePath(in.Path); problem != "" {
		fields["path"] = problem
	}
	if in.Details, problem = cleanText(in.Details, MaxMessageBytes); problem != "" {
		fields["details"] = problem
	}
	if len(fields) > 0 {
		return &InputError{Fields: fields, Message: "Check the highlighted fields."}
	}
	return s.write(ctx, a, func(sc *scope) error {
		if !sc.open {
			return ErrClosed
		}
		t, err := lockThread(ctx, sc, number)
		if err != nil {
			return err
		}
		m, err := lockMessage(ctx, sc, t.ID, position)
		if err != nil {
			return err
		}
		if m.AuthorUserID != a.UserID {
			return ErrUnavailable
		}
		if t.State == pages.StateRemoved || m.Removed {
			return ErrGone
		}
		if position > 1 {
			in.Path, in.Details = "", ""
		}
		if m.Body == in.Body && m.PagePath == in.Path && m.Details == in.Details {
			return nil // nothing changed, or a repeated save
		}
		if m.Revision != in.Revision {
			return &ConflictError{Kind: "edited", Revision: m.Revision}
		}
		if err := sc.q.EditMessage(ctx, dbgen.EditMessageParams{ID: m.ID, Body: in.Body, PagePath: optionalText(in.Path), Details: optionalText(in.Details)}); err != nil {
			return fmt.Errorf("save the edit: %w", err)
		}
		sc.wrote = true
		// One edit event per message per ten minutes: corrections in quick
		// succession are one change on a timeline.
		bucket := s.now().Unix() / 600
		return s.record(ctx, sc, Event{Kind: KindMessageEdited, Metadata: map[string]any{"finding": number, "message": position},
			DedupKey: fmt.Sprintf("message:%d:edited:%d", m.ID, bucket)})
	})
}

// RemoveMessage erases a message: the writer's own, or anyone's by a person
// who can build the pull request, as moderation with a reason. Removing a
// finding's opening message when no visible reply depends on it removes the
// whole finding. It reports whether that happened.
func (s *Service) RemoveMessage(ctx context.Context, a Actor, number, position int32, reason string) (bool, error) {
	var whole bool
	err := s.write(ctx, a, func(sc *scope) error {
		whole = false
		t, err := lockThread(ctx, sc, number)
		if err != nil {
			return err
		}
		m, err := lockMessage(ctx, sc, t.ID, position)
		if err != nil {
			return err
		}
		if t.State == pages.StateRemoved || m.Removed {
			return nil // already removed: a repeated request
		}
		moderated := m.AuthorUserID != a.UserID
		if moderated && !sc.operate {
			return ErrUnavailable
		}
		var why pgtype.Text
		if moderated {
			if !validReason(reason) {
				return &InputError{Fields: map[string]string{"reason": "Choose why you are removing it."}, Message: "Choose why you are removing it."}
			}
			why = optionalText(reason)
		}
		if position == 1 && t.VisibleReplies == 0 {
			if err := sc.q.RemoveThreadMessages(ctx, dbgen.RemoveThreadMessagesParams{ThreadID: t.ID, RemovedBy: a.UserID, RemovalReason: why}); err != nil {
				return fmt.Errorf("remove the finding's messages: %w", err)
			}
			if err := sc.q.SetThreadState(ctx, dbgen.SetThreadStateParams{ID: t.ID, State: pages.StateRemoved, ChangedBy: a.UserID, RemovalReason: why}); err != nil {
				return fmt.Errorf("remove the finding: %w", err)
			}
			whole, sc.wrote = true, true
			return s.record(ctx, sc, Event{Kind: KindFindingRemoved, Metadata: removalMetadata(number, 0, moderated, reason),
				DedupKey: fmt.Sprintf("finding:%d:removed", t.ID)})
		}
		if err := sc.q.RemoveMessage(ctx, dbgen.RemoveMessageParams{ID: m.ID, RemovedBy: a.UserID, RemovalReason: why}); err != nil {
			return fmt.Errorf("remove the message: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindMessageRemoved, Metadata: removalMetadata(number, position, moderated, reason),
			DedupKey: fmt.Sprintf("message:%d:removed", m.ID)})
	})
	return whole, err
}

// RemoveFinding removes a whole finding and erases its messages, as
// moderation by a person who can build the pull request.
func (s *Service) RemoveFinding(ctx context.Context, a Actor, number int32, reason string) error {
	if !validReason(reason) {
		return &InputError{Fields: map[string]string{"reason": "Choose why you are removing it."}, Message: "Choose why you are removing it."}
	}
	return s.write(ctx, a, func(sc *scope) error {
		if !sc.operate {
			return ErrUnavailable
		}
		t, err := lockThread(ctx, sc, number)
		if err != nil {
			return err
		}
		if t.State == pages.StateRemoved {
			return nil
		}
		why := optionalText(reason)
		if err := sc.q.RemoveThreadMessages(ctx, dbgen.RemoveThreadMessagesParams{ThreadID: t.ID, RemovedBy: a.UserID, RemovalReason: why}); err != nil {
			return fmt.Errorf("remove the finding's messages: %w", err)
		}
		if err := sc.q.SetThreadState(ctx, dbgen.SetThreadStateParams{ID: t.ID, State: pages.StateRemoved, ChangedBy: a.UserID, RemovalReason: why}); err != nil {
			return fmt.Errorf("remove the finding: %w", err)
		}
		sc.wrote = true
		return s.record(ctx, sc, Event{Kind: KindFindingRemoved, Metadata: removalMetadata(number, 0, true, reason),
			DedupKey: fmt.Sprintf("finding:%d:removed", t.ID)})
	})
}

func removalMetadata(number, position int32, moderated bool, reason string) map[string]any {
	m := map[string]any{"finding": number, "moderated": moderated}
	if position > 0 {
		m["message"] = position
	}
	if moderated {
		m["reason"] = reason
	}
	return m
}

// Resolve marks a finding resolved: its author or a person who can build the
// pull request. It records the deployment the page showed only while the
// preview still serves it, since that is what could be verified.
func (s *Service) Resolve(ctx context.Context, a Actor, number int32, revision int64, shown int32) error {
	return s.setState(ctx, a, number, revision, pages.StateResolved, func(sc *scope) (int64, error) {
		current, err := sc.q.CurrentPublication(ctx, sc.pr)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read the current deployment: %w", err)
		}
		if current.Serving && current.Sequence == shown {
			return current.ID, nil
		}
		return 0, nil
	})
}

// Reopen marks a resolved finding open again.
func (s *Service) Reopen(ctx context.Context, a Actor, number int32, revision int64) error {
	return s.setState(ctx, a, number, revision, pages.StateOpen, nil)
}

func (s *Service) setState(ctx context.Context, a Actor, number int32, revision int64, state string, verified func(*scope) (int64, error)) error {
	return s.write(ctx, a, func(sc *scope) error {
		if !sc.open {
			return ErrClosed
		}
		t, err := lockThread(ctx, sc, number)
		if err != nil {
			return err
		}
		if t.AuthorUserID != a.UserID && !sc.operate {
			return ErrUnavailable
		}
		switch {
		case t.State == pages.StateRemoved:
			return ErrGone
		case t.State == state:
			return nil // already there: a repeated request or a teammate's identical change
		case t.Revision != revision:
			return ErrStale
		}
		var publication int64
		if verified != nil {
			if publication, err = verified(sc); err != nil {
				return err
			}
		}
		if err := sc.q.SetThreadState(ctx, dbgen.SetThreadStateParams{ID: t.ID, State: state, ChangedBy: a.UserID, ResolvedPublicationID: optionalID(publication)}); err != nil {
			return fmt.Errorf("save the finding's state: %w", err)
		}
		sc.wrote = true
		kind := KindFindingReopened
		if state == pages.StateResolved {
			kind = KindFindingResolved
		}
		return s.record(ctx, sc, Event{Kind: kind, PublicationID: publication, Metadata: map[string]any{"finding": number},
			DedupKey: fmt.Sprintf("finding:%d:%s:%d", t.ID, state, t.Revision+1)})
	})
}

func lockThread(ctx context.Context, sc *scope, number int32) (dbgen.LockThreadRow, error) {
	t, err := sc.q.LockThread(ctx, dbgen.LockThreadParams{PullRequestID: sc.pr, Number: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrUnavailable
	}
	if err != nil {
		return t, fmt.Errorf("lock the finding: %w", err)
	}
	return t, nil
}

func lockMessage(ctx context.Context, sc *scope, thread int64, position int32) (dbgen.LockMessageRow, error) {
	m, err := sc.q.LockMessage(ctx, dbgen.LockMessageParams{ThreadID: thread, Position: position})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrUnavailable
	}
	if err != nil {
		return m, fmt.Errorf("lock the message: %w", err)
	}
	return m, nil
}

func optionalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
func optionalID(id int64) pgtype.Int8   { return pgtype.Int8{Int64: id, Valid: id > 0} }
