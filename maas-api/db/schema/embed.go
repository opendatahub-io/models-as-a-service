// Package schema provides embedded database schema definition files.
// These can be used by the application and e2e tests.
package schema

import "embed"

//go:embed *.sql
var FS embed.FS
