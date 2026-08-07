// Package migrations exposes the embedded PostgreSQL schema migrations.
package migrations

import "embed"

// Files contains all SQL migration files so tests and migration tooling can use
// the exact same schema source.
//
//go:embed *.sql
var Files embed.FS
