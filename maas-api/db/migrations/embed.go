// Package migrations provides embedded database migration files.
// These can be used by the application and e2e tests.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
