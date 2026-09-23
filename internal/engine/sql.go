package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
)

// runSQLStatement executes a side-effectful SQL statement (e.g. CHECKPOINT,
// VACUUM ANALYZE, pg_stat_statements_reset()) against the target database.
// Unlike collectTargetInfo, it ignores any result rows and is intended for
// statements whose effect is on server state, not the returned data.
func runSQLStatement(ctx context.Context, out io.Writer, outputs Outputs, name, statement string) error {
	if statement == "" {
		return fmt.Errorf("sql.%s: empty query", name)
	}
	connStr, err := buildConnString(outputs)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return fmt.Errorf("connect to target: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, statement); err != nil {
		return fmt.Errorf("sql.%s: %w", name, err)
	}
	if out != nil {
		fmt.Fprintf(out, "  %s: ran %q\n", name, statement)
	}
	return nil
}
