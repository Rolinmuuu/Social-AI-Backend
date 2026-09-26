// Package migrations embeds the SQL schema migrations, applied in file-name order by
// shared/db.Migrate (run by cmd/migrate before the services start).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
