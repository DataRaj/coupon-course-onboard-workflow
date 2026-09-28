// Package migrations embeds the SQL schema files so the binaries are self-contained.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
