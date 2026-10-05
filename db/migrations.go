// Package db embeds the Goose migrations so release images apply exactly the
// schema they were built with (cmd/migrate).
package db

import "embed"

// Migrations holds migrations/*.sql.
//
//go:embed migrations/*.sql
var Migrations embed.FS
