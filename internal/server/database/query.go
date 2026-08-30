package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidQueryPath is returned when a query names a path SQLite cannot parse.
	ErrInvalidQueryPath = errors.New("invalid query path")
)

type (
	// The Query type matches rows whose stored JSON has the given value at the
	// given path.
	//
	// The path is a SQLite JSON path. For a workload it is a path into the whole
	// stored specification, so a query can reach anything the specification holds
	// — including labels, which live under $.labels. A volume, secret or variable
	// stores only labels, and its queries reach them under the same $.labels
	// root. One path syntax then works across every resource.
	Query struct {
		// The JSON path into the resource, such as "$.labels.app".
		Path string
		// The value the path must hold, compared as text.
		Value string
	}
)

// labelSource is the JSON source expression for resources whose only queryable
// JSON is their labels column. Wrapping the column under a "labels" key is what
// lets the workload's $.labels.app path reach the labels here too.
const labelSource = `json_object('labels', json(labels))`

// filter builds the WHERE clause matching every query against the JSON the
// source expression produces, along with the clause's arguments.
//
// Each comparison is made as text so that a caller which only has strings — a CLI, a
// URL query parameter — matches a number in the JSON as readily as a string. The
// path is bound as a parameter rather than interpolated, so a query cannot reach
// beyond the value it is inspecting. The source is not a parameter: it is one of the
// constant expressions this package names, never caller input.
func filter(source string, queries []Query) (string, []any) {
	if len(queries) == 0 {
		return "", nil
	}

	clauses := make([]string, 0, len(queries))
	args := make([]any, 0, len(queries)*2)

	for _, query := range queries {
		clauses = append(clauses, "CAST(json_extract("+source+", ?) AS TEXT) = ?")
		args = append(args, query.Path, query.Value)
	}

	return "\n\t\tWHERE " + strings.Join(clauses, "\n\t\t  AND "), args
}

// validPaths reports whether SQLite can parse every query's path.
//
// A malformed path fails the whole query, which would otherwise surface as an
// internal error well after the caller could have been told they mistyped something.
// The check asks SQLite to parse each path against an empty object: it needs no data,
// and agreeing with the engine that will run the query is the point — a hand-written
// approximation would accept paths the query then rejects.
func validPaths(ctx context.Context, db *sql.DB, queries []Query) error {
	const q = `SELECT json_extract('{}', ?)`

	for _, query := range queries {
		var ignored sql.NullString
		if err := db.QueryRowContext(ctx, q, query.Path).Scan(&ignored); err != nil {
			return fmt.Errorf("%w: %q", ErrInvalidQueryPath, query.Path)
		}
	}

	return nil
}
