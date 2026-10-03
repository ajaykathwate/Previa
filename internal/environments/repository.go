package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments/dbgen"
	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Artifact struct {
	ContainerID string `json:"container_id,omitempty"`
	ImageTag    string `json:"image_tag,omitempty"`
}

type Result struct {
	Status        string   `json:"status"`
	Artifact      Artifact `json:"artifact,omitempty"`
	ErrorCode     string   `json:"error_code,omitempty"`
	ReportFailure bool     `json:"report_failure,omitempty"`
}

type Snapshot struct {
	Generation, AppliedGeneration              int64
	Revision, AppliedRevision, PendingRevision int64
	Status, State, HeadSHA                     string
	Artifact, PendingArtifact                  Artifact
	PendingGeneration                          int64
}

type Repository struct {
	pool       *pgxpool.Pool
	operations *OperationBroker
}

func NewRepository(pool *pgxpool.Pool, operations ...*OperationBroker) *Repository {
	r := &Repository{pool: pool}
	if len(operations) > 0 {
		r.operations = operations[0]
	}
	return r
}

// Snapshot is an optimization only. Apply always performs its own locked SQL guard.
func (r *Repository) Snapshot(ctx context.Context, id int64) (Snapshot, error) {
	q := dbgen.New(r.pool)
	pr, err := q.ReadRequest(ctx, id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read PR generation: %w", err)
	}
	s := Snapshot{Generation: pr.Generation, State: pr.State, HeadSHA: pr.HeadSha}
	i, err := q.ReadBuildIntent(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("read current intent: %w", err)
	}
	s.Revision = i.Revision
	e, err := q.ReadEnvironment(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("read environment: %w", err)
	}
	if err == nil {
		s.AppliedGeneration = e.AppliedGeneration
		s.AppliedRevision = e.AppliedRevision
		s.PendingRevision = e.PendingRevision.Int64
		s.Status = e.Status
		s.Artifact = Artifact{ContainerID: e.ContainerID.String, ImageTag: e.ImageTag.String}
		s.PendingArtifact = Artifact{ContainerID: e.PendingContainerID.String, ImageTag: e.PendingImageTag.String}
		if e.PendingGeneration.Valid {
			s.PendingGeneration = e.PendingGeneration.Int64
		}
	}
	return s, nil
}

// Apply checks the generation, updates only database state, and creates durable
// publication intent plus a River wake-up in the same short transaction. External
// systems are never called from this method or while its PR lock is held.
func (r *Repository) Apply(ctx context.Context, request Request, result Result) (applied bool, err error) {
	if err := request.validate(); err != nil {
		return false, err
	}
	if result.Status != "running" && result.Status != "failed" && result.Status != "torn_down" {
		return false, errors.New("invalid environment result status")
	}
	if r.operations == nil {
		return false, errors.New("environment operation broker is not configured")
	}
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		owner, err := q.FindPROwner(ctx, request.PullRequestID)
		if err != nil {
			return fmt.Errorf("find result owner: %w", err)
		}
		scope, err := lockIntentScope(ctx, q, owner.Int64, request.PullRequestID)
		if err != nil {
			if errors.Is(err, ErrIntentUnavailable) {
				return nil
			}
			return err
		}
		if request.State == "open" {
			if _, err := q.ClaimBuildIntent(ctx, dbgen.ClaimBuildIntentParams{PrID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision, HeadSha: request.HeadSHA, BuildMode: scope.mode, NowAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
				return fmt.Errorf("admit completion intent: %w", err)
			}
		}
		// Take the PR lock before executing the SQL: a same-generation Retry
		// may have changed the intent while this transaction was waiting.
		previous, err := q.ApplyResult(ctx, dbgen.ApplyResultParams{
			PrID: request.PullRequestID, ExpectedGeneration: request.Generation, ExpectedRevision: request.Revision,
			ResultStatus: result.Status,
			ContainerID:  pgtype.Text{String: result.Artifact.ContainerID, Valid: result.Artifact.ContainerID != ""},
			ImageTag:     pgtype.Text{String: result.Artifact.ImageTag, Valid: result.Artifact.ImageTag != ""},
			HeadSha:      request.HeadSHA,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			latest, readErr := q.ReadRequest(ctx, request.PullRequestID)
			if readErr != nil {
				return fmt.Errorf("read rejected result generation: %w", readErr)
			}
			current, err := q.ReadBuildIntent(ctx, request.PullRequestID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read rejected result revision: %w", err)
			}
			if latest.Generation == request.Generation && latest.State == request.State && latest.HeadSha == request.HeadSHA && (request.State != "open" || (current.Revision == request.Revision && (current.State == "running" || current.State == "succeeded"))) {
				return errors.New("no active environment provisioned for current request")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply guarded environment result: %w", err)
		}
		applied = true
		prior := Artifact{ContainerID: previous.ContainerID.String, ImageTag: previous.ImageTag.String}
		priorPending := Artifact{ContainerID: previous.PendingContainerID.String, ImageTag: previous.PendingImageTag.String}
		var kind OperationKind
		payload := OperationPayload{Request: request, Result: result, Previous: prior}
		switch result.Status {
		case "running":
			kind = ActivateRoute
			payload.Artifact = result.Artifact
			payload.PreviousPending = priorPending
		case "failed":
			if result.ReportFailure {
				kind = ReportStatus
				payload.ErrorCode = result.ErrorCode
			}
		case "torn_down":
			kind = RemoveRoute
			payload.Artifact = priorPending
		}
		if kind != "" {
			key := operationIdempotencyKey(request, kind, "")
			if _, err := r.operations.EnqueueTx(ctx, tx, OperationSpec{
				Request: request, Kind: kind, IdempotencyKey: key, Payload: payload,
			}); err != nil {
				return fmt.Errorf("persist %s operation: %w", kind, err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// ScheduleResourceCleanup snapshots runtime inventory before entering a DB
// transaction. The transaction only verifies ownership/references and persists
// cleanup operations; deletion is performed later by an operation worker.
func (r *Repository) ScheduleResourceCleanup(ctx context.Context, request Request, resources []Resource) error {
	return database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		latest, err := q.LockRequest(ctx, request.PullRequestID)
		if err != nil {
			return fmt.Errorf("lock PR to schedule resource cleanup: %w", err)
		}
		env, err := q.ReadEnvironment(ctx, request.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read active resources before cleanup: %w", err)
		}
		current := NewRequest(latest.ID, latest.Generation, latest.HeadSha, latest.State)
		i, err := q.ReadBuildIntent(ctx, current.PullRequestID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read cleanup request revision: %w", err)
		}
		current.Revision = i.Revision
		keep := []Artifact{{ContainerID: env.ContainerID.String, ImageTag: env.ImageTag.String}}
		if env.PendingGeneration.Valid {
			keep = append(keep, Artifact{ContainerID: env.PendingContainerID.String, ImageTag: env.PendingImageTag.String})
		}
		protected, err := listProtectedArtifacts(ctx, q, request.PullRequestID)
		if err != nil {
			return err
		}
		keep = append(keep, protected...)
		for _, resource := range resources {
			if resource.PullRequestID != request.PullRequestID || resource.Generation < 0 || resource.Revision < 0 || resource.Generation > current.Generation || (resource.Generation == current.Generation && resource.Revision > current.Revision) {
				continue
			}
			if artifactReferenced(resource.Artifact, keep) {
				continue
			}
			if err := r.enqueueCleanupTx(ctx, tx, current, resource); err != nil {
				return err
			}
		}
		return nil
	})
}

func listProtectedArtifacts(ctx context.Context, q *dbgen.Queries, pullRequestID int64) ([]Artifact, error) {
	payloads, err := q.ListProtectingOperationPayloads(ctx, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts protected by route operations: %w", err)
	}
	var protected []Artifact
	for _, raw := range payloads {
		var payload OperationPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("decode protected route operation payload: %w", err)
		}
		protected = append(protected, payload.Artifact, payload.PreviousPending, payload.Previous)
	}
	return protected, nil
}

func (r *Repository) enqueueCleanupTx(ctx context.Context, tx pgx.Tx, request Request, resource Resource) error {
	if resource.Artifact.ContainerID == "" && resource.Artifact.ImageTag == "" {
		return nil
	}
	discriminator := fmt.Sprintf("generation:%d:revision:%d:container:%s:image:%s", resource.Generation, resource.Revision, resource.Artifact.ContainerID, resource.Artifact.ImageTag)
	payload := OperationPayload{Request: request, Resource: resource}
	key := operationIdempotencyKey(request, CleanupArtifact, discriminator)
	if _, err := r.operations.EnqueueTx(ctx, tx, OperationSpec{
		Request: request, Kind: CleanupArtifact, IdempotencyKey: key, Payload: payload,
	}); err != nil {
		return fmt.Errorf("persist cleanup operation: %w", err)
	}
	return nil
}

func artifactReferenced(resource Artifact, references []Artifact) bool {
	for _, ref := range references {
		if resource.ContainerID != "" && resource.ContainerID == ref.ContainerID {
			return true
		}
		if resource.ImageTag != "" && resource.ImageTag == ref.ImageTag {
			return true
		}
	}
	return false
}

// RecoverOperations recreates missing River wake-up jobs for due durable
// operations and releases expired operation leases. It is safe to run alongside
// workers: active River jobs deduplicate by operation ID, and claims are atomic.
func (r *Repository) RecoverOperations(ctx context.Context, batchSize int32) (requeued int, err error) {
	if batchSize <= 0 {
		return 0, errors.New("operation recovery batch size must be positive")
	}
	if r.operations == nil {
		return 0, errors.New("operation broker is not configured")
	}
	err = database.InTransaction(ctx, r.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		rows, err := q.ListOperationsForRecovery(ctx, batchSize)
		if err != nil {
			return fmt.Errorf("list operations for recovery: %w", err)
		}
		for _, row := range rows {
			if row.State == string(OperationRunning) {
				if _, err := q.ReleaseExpiredOperationLease(ctx, row.ID); err != nil {
					return fmt.Errorf("release expired operation lease %d: %w", row.ID, err)
				}
				row, err = q.GetOperation(ctx, row.ID)
				if err != nil {
					return fmt.Errorf("reload recovered operation: %w", err)
				}
			}
			state := OperationState(row.State)
			if state != OperationPending && state != OperationRetryable {
				continue
			}
			if err := r.operations.enqueueJobTx(ctx, tx, row.ID, OperationKind(row.Kind)); err != nil {
				return err
			}
			requeued++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return requeued, nil
}
