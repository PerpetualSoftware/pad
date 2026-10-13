// Command diagnose-version-ties counts item-version tie groups whose stored
// order breaks the reverse-patch history (BUG-3751). It is a one-off operator
// diagnostic, not a product surface: run it from a checkout against a DSN.
//
//	go run ./scripts/diagnose-version-ties -pg 'postgres://...'
//	go run ./scripts/diagnose-version-ties -sqlite ~/.pad/pad.db
//
// It opens the database WITHOUT running migrations, reads in one transaction
// that the server refuses writes in (READ ONLY on Postgres, query_only on
// SQLite), bounds every statement with -timeout and the whole run with
// -max-duration (a partial run reports "truncated": true), prints progress to
// stderr and the counts as JSON to stdout. It prints no ids, titles or content.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

func main() {
	pg := flag.String("pg", "", "PostgreSQL connection URL")
	sqlite := flag.String("sqlite", "", "SQLite database path")
	timeout := flag.Duration("timeout", 30*time.Second, "per-statement timeout (Postgres)")
	maxDuration := flag.Duration("max-duration", 15*time.Minute, "stop after this long and report what was scanned (bounds the read-only snapshot)")
	flag.Parse()
	if (*pg == "") == (*sqlite == "") {
		fmt.Fprintln(os.Stderr, "pass exactly one of -pg or -sqlite")
		os.Exit(2)
	}
	dsn, postgres := *pg, *pg != ""
	if !postgres {
		dsn = *sqlite
	}
	start := time.Now()
	rep, err := store.DiagnoseVersionTiesAt(context.Background(), dsn, postgres, store.DiagnoseOptions{
		StatementTimeout: *timeout,
		MaxDuration:      *maxDuration,
		Progress: func(done, total int) {
			if done%100 == 0 || done == total {
				fmt.Fprintf(os.Stderr, "%d/%d items with ties (%s)\n", done, total, time.Since(start).Round(time.Second))
			}
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "diagnose-version-ties:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
