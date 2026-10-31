package support

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Incident struct {
	ID, Component, Summary, State string
	Revision                      int64
	Updated                       time.Time
}
type Availability struct {
	Component, State string
	Checked          *time.Time
}

func (s *Service) Status(ctx context.Context) ([]Availability, []Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.component,CASE WHEN count(a.*)=0 THEN 'unknown' WHEN bool_or(a.state='available') THEN 'available' ELSE 'unavailable' END,max(a.checked_at) FROM (VALUES('builds'),('routing')) c(component) LEFT JOIN service_availability a ON a.component=c.component AND a.checked_at>clock_timestamp()-interval '90 seconds' GROUP BY c.component ORDER BY c.component`)
	if err != nil {
		return nil, nil, fmt.Errorf("read service observations: %w", err)
	}
	availability, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Availability])
	if err != nil {
		return nil, nil, fmt.Errorf("decode service observations: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT id::text,component,summary,state,revision,updated_at FROM service_incidents WHERE state<>'resolved' OR updated_at>clock_timestamp()-interval '30 days' ORDER BY updated_at DESC LIMIT 50`)
	if err != nil {
		return nil, nil, fmt.Errorf("list public incidents: %w", err)
	}
	incidents, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Incident])
	if err != nil {
		return nil, nil, fmt.Errorf("read public incidents: %w", err)
	}
	return availability, incidents, nil
}
func (s *Service) Incident(ctx context.Context, github int64, id, component, summary, state string, revision int64) error {
	summary = strings.TrimSpace(summary)
	if !s.Operator(github) {
		return ErrUnavailable
	}
	if !oneOf(component, "builds", "routing", "control_plane") || !oneOf(state, "investigating", "monitoring", "resolved") || len(summary) == 0 || len(summary) > 2000 || !utf8.ValidString(summary) {
		return ErrInput
	}
	action := "incident_update"
	if id == "" {
		id = uuid.NewString()
		action = "incident_create"
	} else if _, err := uuid.Parse(id); err != nil {
		return ErrInput
	}
	return database.InTransaction(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if action == "incident_create" {
			_, err = tx.Exec(ctx, `INSERT INTO service_incidents(id,component,summary,state) VALUES($1,$2,$3,$4)`, id, component, summary, state)
		} else {
			var tag interface{ RowsAffected() int64 }
			tag, err = tx.Exec(ctx, `UPDATE service_incidents SET component=$2,summary=$3,state=$4,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 AND revision=$5`, id, component, summary, state, revision)
			if err == nil && tag.RowsAffected() != 1 {
				return ErrUnavailable
			}
		}
		if err != nil {
			return fmt.Errorf("save public incident: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operator_activity(github_id,action,reference) VALUES($1,$2,$3)`, github, action, id); err != nil {
			return fmt.Errorf("audit public incident: %w", err)
		}
		return nil
	})
}

// Observe publishes only actual dependency probe results. Stale reports are
// unknown; no uptime percentage or guaranteed end-to-end health is inferred.
func (s *Service) Observe(ctx context.Context, worker, component string, available bool) error {
	state := "unavailable"
	if available {
		state = "available"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO service_availability(worker_id,component,state) VALUES($1,$2,$3) ON CONFLICT(worker_id,component) DO UPDATE SET state=EXCLUDED.state,checked_at=clock_timestamp()`, worker, component, state)
	if err != nil {
		return fmt.Errorf("record service probe: %w", err)
	}
	return nil
}
