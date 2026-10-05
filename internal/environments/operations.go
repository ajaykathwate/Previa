package environments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type OperationKind string

const (
	ActivateRoute   OperationKind = "activate_route"
	RemoveRoute     OperationKind = "remove_route"
	ReportStatus    OperationKind = "report_status"
	CleanupArtifact OperationKind = "cleanup_artifact"

	RouteOperationQueue   = "environment_route"
	ReportOperationQueue  = "environment_report"
	CleanupOperationQueue = "environment_cleanup"
)

type OperationState string

const (
	OperationPending    OperationState = "pending"
	OperationRunning    OperationState = "running"
	OperationRetryable  OperationState = "retryable"
	OperationSucceeded  OperationState = "succeeded"
	OperationSuperseded OperationState = "superseded"
	OperationFailed     OperationState = "failed"
)

// OperationPayload contains stable, non-secret references needed to retry an
// external action. The adapter must fetch credentials from its own configuration.
type OperationPayload struct {
	Request         Request  `json:"request"`
	Result          Result   `json:"result,omitempty"`
	Previous        Artifact `json:"previous,omitempty"`
	PreviousPending Artifact `json:"previous_pending,omitempty"`
	Artifact        Artifact `json:"artifact,omitempty"`
	Resource        Resource `json:"resource,omitempty"`
	ErrorCode       string   `json:"error_code,omitempty"`
}

type Operation struct {
	ID             int64
	PullRequestID  int64
	Generation     int64
	Revision       int64
	Kind           OperationKind
	IdempotencyKey string
	Payload        OperationPayload
	State          OperationState
	Attempts       int32
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseExpiresAt time.Time
	LastErrorCode  string
}

type OperationSpec struct {
	Request        Request
	Kind           OperationKind
	IdempotencyKey string
	Payload        OperationPayload
}

type OperationArgs struct {
	OperationID   int64         `json:"operation_id" river:"unique"`
	OperationKind OperationKind `json:"operation_kind"`
}

func (OperationArgs) Kind() string { return "environment_operation" }

func (a OperationArgs) InsertOpts() river.InsertOpts {
	queueName := operationQueue(a.OperationKind)
	return river.InsertOpts{
		Queue: queueName, MaxAttempts: 5,
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending,
			rivertype.JobStateRunning, rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		}},
	}
}

func operationQueue(kind OperationKind) string {
	switch kind {
	case ActivateRoute, RemoveRoute:
		return RouteOperationQueue
	case ReportStatus:
		return ReportOperationQueue
	case CleanupArtifact:
		return CleanupOperationQueue
	default:
		return ""
	}
}

func (kind OperationKind) valid() bool { return operationQueue(kind) != "" }

// retriedUntilDone reports whether an operation kind is idempotent and must
// eventually happen, so exhausting River attempts leaves it retryable. A
// report converges GitHub on the PR's current state, which an outage only
// delays.
func (kind OperationKind) retriedUntilDone() bool {
	return kind == RemoveRoute || kind == CleanupArtifact || kind == ReportStatus
}

type operationInserter interface {
	InsertTx(context.Context, pgx.Tx, river.JobArgs, *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// OperationBroker creates a durable operation and its River wake-up job in the
// caller's transaction. The operation row remains after River job retention.
type OperationBroker struct{ client operationInserter }

func NewOperationBroker(client operationInserter) (*OperationBroker, error) {
	if client == nil {
		return nil, errors.New("operation broker requires a River client")
	}
	return &OperationBroker{client: client}, nil
}

func (b *OperationBroker) EnqueueTx(ctx context.Context, tx pgx.Tx, spec OperationSpec) (int64, error) {
	if b == nil || b.client == nil {
		return 0, errors.New("operation broker is not configured")
	}
	if err := spec.Request.validate(); err != nil {
		return 0, err
	}
	if !spec.Kind.valid() {
		return 0, fmt.Errorf("invalid environment operation kind %q", spec.Kind)
	}
	if spec.IdempotencyKey == "" {
		return 0, errors.New("environment operation requires an idempotency key")
	}
	spec.Payload.Request = spec.Request
	payload, err := json.Marshal(spec.Payload)
	if err != nil {
		return 0, fmt.Errorf("encode operation payload: %w", err)
	}
	q := dbgen.New(tx)
	op, err := q.CreateOperation(ctx, dbgen.CreateOperationParams{
		PullRequestID:  spec.Request.PullRequestID,
		Generation:     spec.Request.Generation,
		Revision:       spec.Request.Revision,
		Kind:           string(spec.Kind),
		IdempotencyKey: spec.IdempotencyKey,
		Payload:        payload,
	})
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("create environment operation: %w", err)
	}
	if !created {
		op, err = q.FindOperationByKey(ctx, spec.IdempotencyKey)
		if err != nil {
			return 0, fmt.Errorf("read existing environment operation: %w", err)
		}
		if op.PullRequestID != spec.Request.PullRequestID || op.Generation != spec.Request.Generation || op.Revision != spec.Request.Revision || op.Kind != string(spec.Kind) || !sameJSON(op.Payload, payload) {
			return 0, errors.New("environment operation idempotency key conflicts with different work")
		}
	}
	id := op.ID
	if !created && spec.Kind == CleanupArtifact && OperationState(op.State) == OperationFailed {
		// Scheduling the same resource again retries a cleanup that failed.
		if _, err := q.RearmCleanupOperation(ctx, id); err != nil {
			return 0, fmt.Errorf("rearm failed cleanup operation: %w", err)
		}
		op.State = string(OperationPending)
	}
	if !created && OperationState(op.State) != OperationPending && OperationState(op.State) != OperationRetryable && OperationState(op.State) != OperationRunning {
		return id, nil
	}
	if err := b.enqueueJobTx(ctx, tx, id, spec.Kind); err != nil {
		return 0, err
	}
	return id, nil
}

func (b *OperationBroker) enqueueJobTx(ctx context.Context, tx pgx.Tx, id int64, kind OperationKind) error {
	args := OperationArgs{OperationID: id, OperationKind: kind}
	job, err := b.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return fmt.Errorf("enqueue environment operation %d: %w", id, err)
	}
	if job.Job.Kind != args.Kind() {
		return fmt.Errorf("operation %d collided with unexpected job kind %q", id, job.Job.Kind)
	}
	return nil
}

func operationIdempotencyKey(request Request, kind OperationKind, discriminator string) string {
	sum := sha256.Sum256([]byte(discriminator))
	return fmt.Sprintf("pr:%d:g:%d:r:%d:%s:%s", request.PullRequestID, request.Generation, request.Revision, kind, hex.EncodeToString(sum[:12]))
}

func operationFromRow(row dbgen.EnvironmentOperation) (Operation, error) {
	var payload OperationPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return Operation{}, fmt.Errorf("decode operation %d payload: %w", row.ID, err)
	}
	if !OperationKind(row.Kind).valid() || payload.Request.PullRequestID != row.PullRequestID || payload.Request.Generation != row.Generation || payload.Request.Revision != row.Revision {
		return Operation{}, fmt.Errorf("operation %d has inconsistent ownership data", row.ID)
	}
	op := Operation{
		ID: row.ID, PullRequestID: row.PullRequestID, Generation: row.Generation, Revision: row.Revision,
		Kind: OperationKind(row.Kind), IdempotencyKey: row.IdempotencyKey,
		Payload: payload, State: OperationState(row.State), Attempts: row.Attempts,
	}
	if row.AvailableAt.Valid {
		op.AvailableAt = row.AvailableAt.Time
	}
	if row.LeaseOwner.Valid {
		op.LeaseOwner = row.LeaseOwner.String
	}
	if row.LeaseExpiresAt.Valid {
		op.LeaseExpiresAt = row.LeaseExpiresAt.Time
	}
	if row.LastErrorCode.Valid {
		op.LastErrorCode = row.LastErrorCode.String
	}
	return op, nil
}

// sameJSON compares JSON documents by value. PostgreSQL returns JSONB in its
// own normalized form, so stored and freshly encoded bytes differ in spacing
// and key order even when they describe the same operation.
func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	normalizedLeft, errLeft := json.Marshal(left)
	normalizedRight, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && string(normalizedLeft) == string(normalizedRight)
}
