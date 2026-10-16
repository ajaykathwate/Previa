package reviews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/reviews/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/reviews"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Activity records a timeline event in the caller's transaction, so it
// commits exactly with the change; internal/app connects the activity
// feature. A nil Activity records nothing.
type Activity interface {
	RecordTx(ctx context.Context, tx pgx.Tx, e Event) error
}

// Event is one review change for the timeline. Metadata stays small and never
// holds message text.
type Event struct {
	PullRequestID, PublicationID, ActorID int64
	Kind                                  string
	Metadata                              map[string]any
	DedupKey                              string
}

// Event kinds this feature records.
const (
	KindInstructionsUpdated = "review.instructions_updated"
	KindFindingCreated      = "feedback.thread_created"
	KindFindingRemoved      = "feedback.thread_removed"
	KindFindingResolved     = "feedback.thread_resolved"
	KindFindingReopened     = "feedback.thread_reopened"
	KindReplyAdded          = "feedback.reply_added"
	KindMessageEdited       = "feedback.message_edited"
	KindMessageRemoved      = "feedback.message_removed"
)

// Options configure the service.
type Options struct {
	PreviewDomain  string // previews' base domain; empty: no preview addresses
	WriteAllowance int    // review changes per member per hour; 0: DefaultWriteAllowance
}

// Service reads and changes pull request reviews. Every query derives access
// from the repository; nothing trusts a selected workspace or a form's claim.
type Service struct {
	pool     *pgxpool.Pool
	activity Activity
	opts     Options
	now      func() time.Time
}

// NewService returns the reviews service.
func NewService(pool *pgxpool.Pool, activity Activity, opts Options) (*Service, error) {
	if pool == nil {
		return nil, errors.New("reviews need a database")
	}
	if opts.WriteAllowance <= 0 {
		opts.WriteAllowance = DefaultWriteAllowance
	}
	return &Service{pool: pool, activity: activity, opts: opts, now: time.Now}, nil
}

// Query is what the page lists: a filtered page of findings, or one finding
// by its own link.
type Query struct {
	State      string // open (default), resolved or all
	Author     int64
	AuthorKey  string // the author's public ID, to keep in addresses
	Deployment int32
	Search     string
	Page       int64
	Finding    int32
}

// Page is a pull request's review page for userID.
func (s *Service) Page(ctx context.Context, userID, repoID int64, number int32, query Query) (pages.Page, error) {
	q := dbgen.New(s.pool)
	t, err := q.ReviewTarget(ctx, dbgen.ReviewTargetParams{UserID: userID, RepoID: repoID, Number: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return pages.Page{}, ErrUnavailable
	}
	if err != nil {
		return pages.Page{}, fmt.Errorf("load the review: %w", err)
	}
	p := pages.Page{
		RepoID: repoID, Repository: t.FullName, Number: t.Number, Title: t.Title, Author: t.Author, Branch: t.HeadBranch,
		State: t.State, GitHubURL: githubURL(t.HtmlUrl), Description: strings.TrimSpace(t.Description),
		CanOperate: t.CanOperate, ViewerID: userID, OpenFindings: t.OpenFindings,
		Preview: s.preview(t),
	}
	ids := map[int64]bool{}
	instructions, err := q.GetInstructions(ctx, t.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return pages.Page{}, fmt.Errorf("load the testing instructions: %w", err)
	default:
		ids[instructions.UpdatedBy] = true
	}
	list, err := s.list(ctx, q, t.ID, query)
	if err != nil {
		return pages.Page{}, err
	}
	for _, row := range list.threads {
		ids[row.AuthorUserID], ids[row.StateChangedBy] = true, true
	}
	for _, m := range list.messages {
		ids[m.AuthorUserID], ids[m.RemovedBy] = true, true
	}
	for _, a := range list.authors {
		ids[a] = true
	}
	people, err := s.people(ctx, q, repoID, userID, ids)
	if err != nil {
		return pages.Page{}, err
	}
	if instructions.Revision > 0 {
		p.Instructions = pages.Instructions{
			Summary: instructions.Summary, Steps: instructions.Steps, Expected: instructions.Expected,
			StartPath: instructions.StartPath, DocsPath: instructions.DocsPath, Revision: instructions.Revision,
			UpdatedBy: people.get(instructions.UpdatedBy), UpdatedAt: stamp(instructions.UpdatedAt),
		}
	}
	p.Findings = list.findings
	p.Findings.ComposerKey = NewRequestKey()
	for _, a := range list.authors {
		p.Findings.Authors = append(p.Findings.Authors, people.get(a))
	}
	counters, err := q.GetCounters(ctx, t.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pages.Page{}, fmt.Errorf("load the review limits: %w", err)
	}
	p.Findings.Limit = MaxThreads
	p.Findings.Full = counters.Threads >= MaxThreads || counters.Messages >= MaxMessages
	messages := map[int64][]dbgen.ListMessagesRow{}
	for _, m := range list.messages {
		messages[m.ThreadID] = append(messages[m.ThreadID], m)
	}
	for _, row := range list.threads {
		th := thread(row, messages[row.ID], people, userID, t.CanOperate, t.State == "open")
		if th.CanReply {
			th.ReplyKey = NewRequestKey()
		}
		p.Findings.Threads = append(p.Findings.Threads, th)
	}
	return p, nil
}

// listing is one page of findings with the rows its people come from.
type listing struct {
	findings pages.Findings
	threads  []dbgen.ListThreadsRow
	messages []dbgen.ListMessagesRow
	authors  []int64
}

func (s *Service) list(ctx context.Context, q *dbgen.Queries, pr int64, query Query) (listing, error) {
	f := pages.Filter{State: query.State, Author: query.Author, AuthorKey: query.AuthorKey, Deployment: query.Deployment, Search: query.Search, Page: max(query.Page, 1)}
	if f.State != pages.StateResolved && f.State != pages.StateAll {
		f.State = pages.StateOpen
	}
	l := listing{findings: pages.Findings{Filter: f, Page: 1, Pages: 1}}
	authors, err := q.ListThreadAuthors(ctx, pr)
	if err != nil {
		return l, fmt.Errorf("list finding authors: %w", err)
	}
	l.authors = authors
	deployments, err := q.ListThreadDeployments(ctx, pr)
	if err != nil {
		return l, fmt.Errorf("list finding deployments: %w", err)
	}
	for _, d := range deployments {
		l.findings.Deployments = append(l.findings.Deployments, pages.Deployment{Sequence: d.Sequence, SHA: d.HeadSha})
	}
	counts, err := q.CountThreads(ctx, dbgen.CountThreadsParams{PullRequestID: pr, AuthorID: f.Author, Deployment: f.Deployment, Search: f.Search})
	if err != nil {
		return l, fmt.Errorf("count findings: %w", err)
	}
	l.findings.Open, l.findings.Resolved, l.findings.All = counts.Open, counts.Resolved, counts.Total
	latest := int32(pages.LatestReplies)
	if query.Finding > 0 {
		l.findings.Focus = query.Finding
		row, err := q.GetThread(ctx, dbgen.GetThreadParams{PullRequestID: pr, Number: query.Finding})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			l.findings.FocusMissing = true
			return l, nil
		case err != nil:
			return l, fmt.Errorf("load the finding: %w", err)
		}
		l.threads = []dbgen.ListThreadsRow{dbgen.ListThreadsRow(row)}
		latest = MaxThreadMessages
	} else {
		switch f.State {
		case pages.StateOpen:
			l.findings.Total = counts.Open
		case pages.StateResolved:
			l.findings.Total = counts.Resolved
		default:
			l.findings.Total = counts.Total
		}
		l.findings.Pages = max(1, (l.findings.Total+pages.PageSize-1)/pages.PageSize)
		l.findings.Page = min(f.Page, l.findings.Pages)
		l.findings.Filter.Page = l.findings.Page
		rows, err := q.ListThreads(ctx, dbgen.ListThreadsParams{
			PullRequestID: pr, State: f.State, AuthorID: f.Author, Deployment: f.Deployment, Search: f.Search,
			PageSize: pages.PageSize, PageOffset: int32((l.findings.Page - 1) * pages.PageSize),
		})
		if err != nil {
			return l, fmt.Errorf("list findings: %w", err)
		}
		l.threads = rows
	}
	ids := make([]int64, len(l.threads))
	for i, row := range l.threads {
		ids[i] = row.ID
	}
	if len(ids) > 0 {
		if l.messages, err = q.ListMessages(ctx, dbgen.ListMessagesParams{ThreadIds: ids, Latest: latest}); err != nil {
			return l, fmt.Errorf("list finding messages: %w", err)
		}
	}
	return l, nil
}

// thread turns a finding's rows into what the viewer sees and may do.
func thread(row dbgen.ListThreadsRow, messages []dbgen.ListMessagesRow, people directory, viewer int64, operate, open bool) pages.Thread {
	t := pages.Thread{
		Number: row.Number, State: row.State, Revision: row.Revision, Author: people.get(row.AuthorUserID),
		CreatedAt: stamp(row.CreatedAt), Deployment: deployment(row.PublicationSequence, row.PublicationSha),
		ResolvedOn: deployment(row.ResolvedSequence, row.ResolvedSha), RemovalReason: row.RemovalReason,
		StateChangedAt: stamp(row.StateChangedAt),
	}
	if row.StateChangedBy > 0 {
		t.StateChangedBy = people.get(row.StateChangedBy)
	}
	removed := row.State == pages.StateRemoved
	total := 0
	for _, m := range messages {
		total = int(m.ThreadMessages)
		msg := pages.Message{
			Position: m.Position, Author: people.get(m.AuthorUserID), Body: m.Body, Path: m.PagePath, Details: m.Details,
			Revision: m.Revision, CreatedAt: stamp(m.CreatedAt), EditedAt: stamp(m.EditedAt), Removed: m.RemovedAt.Valid,
			RemovalReason: m.RemovalReason, Deployment: deployment(m.PublicationSequence, m.PublicationSha),
		}
		if msg.Removed {
			msg.RemovedBy = people.get(m.RemovedBy)
			msg.RemovedByItsAuthor = m.RemovedBy == m.AuthorUserID && m.RemovalReason == ""
			msg.RemovedWithItsThread = removed
		}
		msg.DifferentDeployment = m.Position > 1 && msg.Deployment != nil &&
			(t.Deployment == nil || msg.Deployment.Sequence != t.Deployment.Sequence)
		own := m.AuthorUserID == viewer
		msg.CanEdit = open && own && !msg.Removed && !removed
		msg.CanRemove = !msg.Removed && !removed && (own || operate)
		msg.Moderate = msg.CanRemove && !own
		t.Messages = append(t.Messages, msg)
	}
	t.Earlier = max(0, total-len(t.Messages))
	t.CanReply = open && !removed && total < MaxThreadMessages
	t.CanResolve = open && !removed && (row.AuthorUserID == viewer || operate)
	t.CanRemove = !removed && operate
	t.Moderate = t.CanRemove
	return t
}

// preview describes what the pull request's preview serves now.
func (s *Service) preview(t dbgen.ReviewTargetRow) pages.Preview {
	p := pages.Preview{Latest: t.HeadSha, State: "none"}
	deployed := t.DeployedSha != "" && (t.EnvironmentStatus == "running" || t.EnvironmentStatus == "publishing" || t.EnvironmentStatus == "failed")
	switch {
	case deployed:
		p.Deployed, p.State = t.DeployedSha, "live"
		if t.AsleepAt.Valid {
			p.State = "asleep"
		}
		if t.EnvironmentStatus == "failed" {
			p.State = "previous"
		}
		if t.Hostname != "" && s.opts.PreviewDomain != "" {
			p.Address = "https://" + t.Hostname + "." + s.opts.PreviewDomain
		}
		p.ExpiresAt = stamp(t.ExpiresAt)
		p.DocsPath = t.DocsPath
	case t.PublicationID > 0 && t.TeardownReason == "expired":
		p.State = "expired"
	case t.PublicationID > 0:
		p.State = "removed"
	}
	if t.PublicationID > 0 {
		d := &pages.Deployment{Sequence: t.PublicationSequence, SHA: t.PublicationSha, Backfilled: t.PublicationBackfilled}
		if !d.Backfilled {
			d.PublishedAt = stamp(t.PublicationAt)
		}
		p.Current = d
	}
	if t.State == "open" && t.IntentSha != "" && t.IntentSha != p.Deployed {
		switch t.IntentState {
		case "pending", "queued", "running":
			p.Building = t.IntentSha
		case "failed":
			p.Failed = t.IntentSha == t.HeadSha
		}
	}
	return p
}

// directory names the people in a review.
type directory struct {
	byID   map[int64]pages.Person
	viewer int64
}

func (d directory) get(id int64) pages.Person {
	if p, ok := d.byID[id]; ok {
		return p
	}
	return pages.Person{ID: id, Former: true, You: id == d.viewer}
}

func (s *Service) people(ctx context.Context, q *dbgen.Queries, repoID, viewer int64, wanted map[int64]bool) (directory, error) {
	d := directory{byID: map[int64]pages.Person{}, viewer: viewer}
	var ids []int64
	for id := range wanted {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return d, nil
	}
	rows, err := q.ReviewPeople(ctx, dbgen.ReviewPeopleParams{RepoID: repoID, Ids: ids})
	if err != nil {
		return d, fmt.Errorf("load review participants: %w", err)
	}
	for _, r := range rows {
		avatar := r.AvatarUrl
		if r.CustomAvatar && r.PublicID != "" {
			avatar = fmt.Sprintf("/avatars/%s?v=%d", r.PublicID, r.AvatarVersion)
		}
		d.byID[r.ID] = pages.Person{
			ID: r.ID, Login: r.Username, Name: strings.TrimSpace(r.FirstName + " " + r.LastName), Avatar: avatar,
			Former: !r.Member, You: r.ID == viewer,
		}
	}
	return d, nil
}

func deployment(sequence int32, sha string) *pages.Deployment {
	if sequence <= 0 {
		return nil
	}
	return &pages.Deployment{Sequence: sequence, SHA: sha}
}

func stamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// githubURL keeps only a GitHub web address.
func githubURL(raw string) string {
	if !strings.HasPrefix(raw, "https://github.com/") {
		return ""
	}
	return raw
}
