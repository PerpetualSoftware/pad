package materialize

import (
	"os"
	"testing"
)

// noSkipEnv, set by CI's native-smoke job on macOS and Windows, turns the
// memory-cap tests' skips into failures, so a platform cap that silently
// stopped running cannot pass as "skipped".
const noSkipEnv = "PAD_MATERIALIZE_TEST_NO_SKIP"

func skipCapTest(t *testing.T, why string) {
	t.Helper()
	if os.Getenv(noSkipEnv) != "" {
		t.Fatalf("%s is set, but this test would skip: %s", noSkipEnv, why)
	}
	t.Skip(why)
}
