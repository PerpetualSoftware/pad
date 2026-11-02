package storetest

import (
	"os"
	"testing"
)

// TracePGConnected records a test Postgres connection only when explicitly
// enabled. Call after a successful Ping, query, or migration, never sql.Open.
// helper must be a stable name, with no connection details.
// A nil tb is reserved for template teardown from TestMain, where no live test
// remains to receive a log. Those teardown-only connections cannot be traced
// through testing.TB; per-test and error-path cleanup always pass a live tb.
func TracePGConnected(tb testing.TB, helper string) {
	if os.Getenv("PAD_TEST_PG_TRACE") == "1" && tb != nil {
		tb.Helper()
		tb.Log("PAD_PG_CONNECTED helper=" + helper)
	}
}
