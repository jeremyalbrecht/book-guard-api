// Package migrations holds the SQL schema for the Postgres-backed store,
// embedded into the binary so the same schema is applied by the server at
// startup and by the tests, with no external files to ship or locate at runtime.
package migrations

import (
	"embed"
	"io/fs"
	"sort"
)

// files holds every migration. Applied in lexical filename order (0001, 0002, …),
// so name new migrations with a monotonically increasing prefix.
//
//go:embed *.sql
var files embed.FS

// Migration is one ordered schema step.
type Migration struct {
	Name string
	SQL  string
}

// All returns the migrations in the order they must be applied.
func All() ([]Migration, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	for _, name := range names {
		sql, err := files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Name: name, SQL: string(sql)})
	}
	return out, nil
}
