package buildtools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runTestShard runs scripts/test-shard.sh from the repo root against the
// shardpkg fixture, with spread as its pin file ("" for none).
func runTestShard(t *testing.T, spread string, args ...string) (string, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "spread.txt")
	if spread != "" {
		if err := os.WriteFile(file, []byte(spread), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("scripts/test-shard.sh", args...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "TEST_SHARD_SPREAD="+file)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

const shardFixture = "./internal/buildtools/testdata/shardpkg/"

// TASK-4648: pinned tests are dealt first, one per shard in file order, so the
// heaviest tests stay apart however the sorted list shifts; the rest follow in
// sorted order, and check still proves the shards disjoint and complete.
func TestTestShard_SpreadDealsPinnedTestsFirst(t *testing.T) {
	const pins = "# comment\n\ninternal/buildtools/testdata/shardpkg TestG\n" +
		"internal/buildtools/testdata/shardpkg TestF\n" +
		"internal/other TestA\n" +
		"internal/buildtools/testdata/shardpkg TestE\n"
	// Dealing order G F E A B C D; the other package's pin is ignored.
	want := map[string]string{
		"1": "^(TestG|TestA|TestD)$",
		"2": "^(TestF|TestB)$",
		"3": "^(TestE|TestC)$",
	}
	for k, re := range want {
		out, err := runTestShard(t, pins, "regex", shardFixture, "3", k)
		if err != nil || strings.TrimSpace(out) != re {
			t.Errorf("shard %s/3 = %q (err %v), want %q", k, out, err, re)
		}
	}
	out, err := runTestShard(t, pins, "check", shardFixture, "3")
	if err != nil || !strings.Contains(out, "3 shards, 7 tests, disjoint and complete") {
		t.Errorf("check with pins: err %v\n%s", err, out)
	}

	// No pin file: plain sorted round robin.
	out, err = runTestShard(t, "", "regex", shardFixture, "3", "1")
	if err != nil || strings.TrimSpace(out) != "^(TestA|TestD|TestG)$" {
		t.Errorf("shard 1/3 without pins = %q (err %v), want ^(TestA|TestD|TestG)$", out, err)
	}
}

// A pin that names no test in the package, names one twice, or is not two
// fields fails the run rather than being skipped, so it cannot go stale.
func TestTestShard_SpreadRefusesBadPins(t *testing.T) {
	const pkg = "internal/buildtools/testdata/shardpkg"
	for name, pins := range map[string]string{
		"missing":   pkg + " TestNoSuchTest\n",
		"duplicate": pkg + " TestB\n" + pkg + " TestB\n",
		"malformed": pkg + "\n",
	} {
		for _, args := range [][]string{{"regex", shardFixture, "3", "1"}, {"check", shardFixture, "3"}} {
			if out, err := runTestShard(t, pins, args...); err == nil {
				t.Errorf("%s pin, %s: exit 0, want a failure\n%s", name, args[0], out)
			}
		}
	}
}
