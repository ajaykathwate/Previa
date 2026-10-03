package environments

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type intentStore interface {
	ReadIntent(context.Context, int64, int64) (IntentView, error)
	RequestIntent(context.Context, IntentCommand) (IntentOutcome, error)
	AdvanceIntent(context.Context, IntentTransition) (BuildIntent, error)
	ObserveIntentTx(context.Context, pgx.Tx, int64, int64, time.Time) (ObservationOutcome, error)
}

// IntentService owns authorization semantics independently of runtime adapters.
// It is deliberately not constructed or called by the production app yet.
type IntentService struct{ store intentStore }

func NewIntentService(store intentStore) (*IntentService, error) {
	if store == nil {
		return nil, errors.New("build intent service requires a repository")
	}
	return &IntentService{store: store}, nil
}

func (s *IntentService) Get(ctx context.Context, userID, prID int64) (IntentView, error) {
	if userID <= 0 || prID <= 0 {
		return IntentView{}, ErrIntentUnavailable
	}
	return s.store.ReadIntent(ctx, userID, prID)
}

func (s *IntentService) Request(ctx context.Context, command IntentCommand) (IntentOutcome, error) {
	if err := command.validate(); err != nil {
		return IntentOutcome{}, err
	}
	return s.store.RequestIntent(ctx, command)
}

func (s *IntentService) Advance(ctx context.Context, command IntentTransition) (BuildIntent, error) {
	if err := command.validate(); err != nil {
		return BuildIntent{}, err
	}
	return s.store.AdvanceIntent(ctx, command)
}

// ObserveTx reconciles intent after a trusted state update in the SAME transaction.
// The caller must acquire installation → repo → PR locks BEFORE updating the PR.
// Deadline calculation, delivery deduplication and River wake-ups belong to Prompt 3.
func (s *IntentService) ObserveTx(ctx context.Context, tx pgx.Tx, userID, prID int64, deadline time.Time) (ObservationOutcome, error) {
	if tx == nil || userID <= 0 || prID <= 0 {
		return ObservationOutcome{}, ErrIntentUnavailable
	}
	return s.store.ObserveIntentTx(ctx, tx, userID, prID, deadline)
}
