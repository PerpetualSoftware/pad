package store

import (
	"os"
	"testing"
)

// Duplicated from storetest.TracePGConnected: storetest imports store, so these
// white-box tests cannot import it. Keep the gate and marker format in sync.
// A nil tb is reserved for TestMain template teardown, after tests finish.
func tracePGConnected(tb testing.TB, helper string) {
	if os.Getenv("PAD_TEST_PG_TRACE") == "1" && tb != nil {
		tb.Helper()
		tb.Log("PAD_PG_CONNECTED helper=" + helper)
	}
}
