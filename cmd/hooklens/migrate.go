package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	// Registers the "pgx" driver with database/sql. Imported for its side
	// effect only, hence the blank identifier. goose talks to *sql.DB, not to
	// pgx directly, so this adapter is what connects the two.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/DinithiPramodya/hooklens/migrations"
)

// runMigrate applies, reverses, or reports on schema migrations.
//
// Migrations are embedded in the binary rather than read from disk, so this
// works on a server with nothing deployed but the executable, and the schema can
// never be a different version from the code that expects it.
func runMigrate(ctx context.Context, log *slog.Logger, dsn string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hooklens migrate <up|down|status|version>")
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{log})
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	// database/sql, not pgxpool, purely because goose expects *sql.DB. The
	// application itself will use pgxpool in Phase 1 -- it is a better fit for a
	// long-running server -- but a migration runner opens one connection, does a
	// handful of statements, and exits, so the generic pool costs nothing here.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	// Explicitly discarded rather than `defer db.Close()`. Close can fail, and
	// an unchecked error in a defer is how "the migration succeeded but the
	// connection did not flush" goes unnoticed. Here the process is exiting
	// immediately either way, so discarding is correct -- but saying so beats
	// leaving a reader (and errcheck) to wonder.
	defer func() { _ = db.Close() }()

	// sql.Open does not connect; it only validates the DSN and prepares a pool.
	// Without an explicit ping, a wrong host or password would not surface until
	// the first migration statement, buried under a less obvious error.
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	// "." is the root of the embedded filesystem, where the .sql files live.
	const dir = "."

	switch cmd := args[0]; cmd {
	case "up":
		return goose.UpContext(ctx, db, dir)
	case "down":
		// Reverses exactly one migration. Deliberately not "reset": a command
		// that drops every table should be typed out, not one word away from
		// the one you use daily.
		return goose.DownContext(ctx, db, dir)
	case "status":
		return goose.StatusContext(ctx, db, dir)
	case "version":
		return goose.VersionContext(ctx, db, dir)
	default:
		return fmt.Errorf("unknown migrate command %q (want up, down, status or version)", cmd)
	}
}

// gooseLogger adapts our slog.Logger to the interface goose expects, so
// migration output goes through the same handler as everything else instead of
// straight to stderr in a different format.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info(fmt.Sprintf(format, v...))
}

func (g gooseLogger) Fatalf(format string, v ...any) {
	// Deliberately not os.Exit. goose calls Fatalf on failure, but exiting here
	// would skip every deferred function on the way out -- including db.Close.
	// Panicking unwinds them, and run() has no recover, so the process still
	// dies with a non-zero status.
	panic(fmt.Sprintf(format, v...))
}
