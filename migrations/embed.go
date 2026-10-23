// Package migrations embeds the SQL migrations. Each file starts with a
// "-- +cms expand" or "-- +cms contract" tag that CI enforces (spec 9.2).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
