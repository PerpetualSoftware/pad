package materialize

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collab"
)

// TASK-3574 (1): three sequential Yjs updates (a paragraph "alpha", then
// " beta", then " gamma" anchored on " beta"), as op-log frames. Generated once
// with yjs (fixed clientID 4242) and pinned as literals.
var task3574Frames = []string{
	"AAIsAQOSIQAHAQdkZWZhdWx0AwlwYXJhZ3JhcGgHAJIhAAYEAJIhAQVhbHBoYQA=",
	"AAIQAQGSIQeEkiEGBSBiZXRhAA==",
	"AAIRAQGSIQyEkiELBiBnYW1tYQA=",
}

func task3574Rows(t *testing.T, idx ...int) [][]byte {
	t.Helper()
	var out [][]byte
	for _, i := range idx {
		b, err := base64.StdEncoding.DecodeString(task3574Frames[i])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// The bundle used to return "alpha" for an op-log missing its middle row: a
// truncated, nonblank body. It now refuses with ErrIncompleteLog, as
// compaction always did.
func TestIncompleteLogIsRefusedNotTruncated(t *testing.T) {
	r := runner(t)
	ctx := context.Background()
	full, err := r.Materialize(ctx, Job{Rows: task3574Rows(t, 0, 1, 2), SchemaVersion: collab.DefaultSchemaVersion})
	if err != nil || strings.TrimSpace(full) != "alpha beta gamma" {
		t.Fatalf("control: the complete log = %q, %v", full, err)
	}
	out, err := r.Materialize(ctx, Job{Rows: task3574Rows(t, 0, 2), SchemaVersion: collab.DefaultSchemaVersion})
	if !errors.Is(err, ErrIncompleteLog) {
		t.Fatalf("an op-log missing a row answered %q, %v; want ErrIncompleteLog", out, err)
	}
	if _, err := r.Snapshot(ctx, Job{Rows: task3574Rows(t, 0, 2), SchemaVersion: collab.DefaultSchemaVersion}); !errors.Is(err, ErrIncompleteLog) {
		t.Fatalf("compaction's refusal is the same typed error; got %v", err)
	}
}
