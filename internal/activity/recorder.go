package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/ajaykathwatee/previa/internal/activity/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Kinds name events as "area.what_happened"; the feature that records an
// event owns its kind (the reviews feature writes "feedback.*" and
// "review.*"). PreviewPublished is written for the environments feature.
const (
	PreviewPublished     = "preview.published"
	ResetRequested       = "preview.reset_requested" // Reset test data: a Rebuild of the serving commit
	VisibilityRepository = "repository"              // everyone who can read the repository
)

// MaxMetadata bounds an event's metadata, encoded as JSON.
const MaxMetadata = 2048

var kindPattern = regexp.MustCompile(`^[a-z]+(_[a-z]+)*\.[a-z]+(_[a-z]+)*$`)

// Event is one thing that happened to a pull request. Its workspace and
// repository derive from the pull request.
type Event struct {
	PullRequestID int64
	PublicationID int64 // the deployment it concerns; 0 for none
	ActorID       int64 // the person who did it; 0 for the system
	Kind          string
	Visibility    string // who may see it; empty means VisibilityRepository
	// Metadata is small, safe context such as a finding's number. Never a
	// message body, secret, token or app output.
	Metadata map[string]any
	// DedupKey makes a retried write record one event; empty never dedupes.
	DedupKey string
}

// Recorder writes events inside the caller's transaction, so an event commits
// exactly when the change it describes does.
type Recorder struct{}

// NewRecorder returns the recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// RecordTx adds e in tx. A repeated DedupKey is not an error.
func (*Recorder) RecordTx(ctx context.Context, tx pgx.Tx, e Event) error {
	if e.PullRequestID <= 0 || !kindPattern.MatchString(e.Kind) {
		return errors.New("invalid activity event")
	}
	visibility := e.Visibility
	if visibility == "" {
		visibility = VisibilityRepository
	}
	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode activity metadata: %w", err)
	}
	if len(encoded) > MaxMetadata {
		return errors.New("activity metadata too large")
	}
	if _, err := dbgen.New(tx).InsertEvent(ctx, dbgen.InsertEventParams{
		PullRequestID: e.PullRequestID, PublicationID: optional(e.PublicationID), ActorUserID: optional(e.ActorID),
		Kind: e.Kind, Visibility: visibility, Metadata: encoded,
		DedupKey: pgtype.Text{String: e.DedupKey, Valid: e.DedupKey != ""},
	}); err != nil {
		return fmt.Errorf("record activity %s: %w", e.Kind, err)
	}
	return nil
}

func optional(id int64) pgtype.Int8 { return pgtype.Int8{Int64: id, Valid: id > 0} }
