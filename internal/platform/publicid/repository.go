// Package publicid translates stable public UUIDs at the HTTP boundary. Resource
// features retain ownership of authorization, transactions and internal keys.
package publicid

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ajaykathwatee/previa/web/components"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalid = errors.New("invalid public resource identifier")
var tables = map[string]string{"workspace": "workspaces", "user": "users", "repository": "repos", "installation": "installations", "invitation": "workspace_invitations", "destination": "repository_destinations", "environment": "environments"}

type Catalog struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Catalog { return &Catalog{pool: pool} }

func (c *Catalog) Resolve(ctx context.Context, kind, value string, parent int64) (int64, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil {
		return 0, ErrInvalid
	}
	var id int64
	if kind == "pull-request" {
		err = c.pool.QueryRow(ctx, "SELECT number FROM pull_requests WHERE public_id=$1 AND repo_id=$2", parsed.String(), parent).Scan(&id)
	} else {
		table, ok := tables[kind]
		if !ok {
			return 0, ErrInvalid
		}
		err = c.pool.QueryRow(ctx, "SELECT id FROM "+table+" WHERE public_id=$1", parsed.String()).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("resolve %s UUID: %w", kind, err)
	}
	return id, nil
}
func (c *Catalog) Load(ctx context.Context, refs []components.ResourceRef) (components.PublicIDs, error) {
	ids := components.PublicIDs{}
	group := map[string][]components.ResourceRef{}
	seen := map[components.ResourceRef]bool{}
	for _, ref := range refs {
		if ref.ID > 0 && !seen[ref] {
			group[ref.Kind] = append(group[ref.Kind], ref)
			seen[ref] = true
		}
	}
	for kind, items := range group {
		if err := c.loadGroup(ctx, kind, items, ids); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
func (c *Catalog) loadGroup(ctx context.Context, kind string, refs []components.ResourceRef, ids components.PublicIDs) error {
	local := make([]int64, 0, len(refs))
	parents := make([]int64, 0, len(refs))
	for _, ref := range refs {
		local = append(local, ref.ID)
		parents = append(parents, ref.Parent)
	}
	table, ok := tables[kind]
	if !ok && kind != "pull-request" {
		return ErrInvalid
	}
	query := "SELECT id,0::bigint,public_id::text FROM " + table + " WHERE id=ANY($1::bigint[])"
	args := []any{local}
	if kind == "pull-request" {
		query = "SELECT pr.number::bigint,pr.repo_id,pr.public_id::text FROM pull_requests pr JOIN unnest($1::bigint[],$2::bigint[]) AS wanted(number,repo) ON pr.number=wanted.number AND pr.repo_id=wanted.repo"
		args = append(args, parents)
	}
	rows, err := c.pool.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read public %s identifiers: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent int64
		var value string
		if err := rows.Scan(&id, &parent, &value); err != nil {
			return fmt.Errorf("scan public %s identifier: %w", kind, err)
		}
		ids[components.ResourceRef{Kind: kind, ID: id, Parent: parent}] = value
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate public %s identifiers: %w", kind, err)
	}
	return nil
}
func (c *Catalog) Public(ctx context.Context, kind string, id, parent int64) (string, error) {
	ids, err := c.Load(ctx, []components.ResourceRef{{Kind: kind, ID: id, Parent: parent}})
	if err != nil {
		return "", err
	}
	return ids.Value(kind, id, parent)
}
func parseLegacy(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid
	}
	return id, nil
}
