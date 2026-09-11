package cli

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestRenderRelationValue covers the three shapes a hydrated relation can
// arrive in, and one thing the renderer must NOT say.
func TestRenderRelationValue(t *testing.T) {
	t.Parallel()

	t.Run("resolved renders ref and title", func(t *testing.T) {
		t.Parallel()
		got := RenderRelationValue(models.RelationTarget{ID: "abc", Ref: "COLO-3", Title: "Red"})
		if got != "COLO-3 · Red" {
			t.Errorf("got %q, want %q", got, "COLO-3 · Red")
		}
		if strings.Contains(got, "abc") {
			t.Error("the raw id leaked into a fully-resolved render, which is the noise U6 exists to remove")
		}
	})

	t.Run("id-only keeps the id and claims nothing about it", func(t *testing.T) {
		t.Parallel()
		got := RenderRelationValue(models.RelationTarget{ID: "6f2c-dead"})
		if !strings.Contains(got, "6f2c-dead") {
			t.Errorf("got %q, want the stored id retained — it is all the caller has", got)
		}
		// THE POINT OF THIS LEG. An id-only entry means either the target is
		// gone or the requester may not see it, and this layer cannot tell
		// which. Saying "deleted" asserts non-existence for a target that may
		// be alive and merely hidden — the existence oracle the server's
		// collapse exists to prevent, and the exact defect codex round 12
		// found in the copy dialog's not_found wording.
		for _, forbidden := range []string{"deleted", "removed", "does not exist", "missing"} {
			if strings.Contains(strings.ToLower(got), forbidden) {
				t.Errorf("render %q claims %q; an id-only entry covers BOTH a gone target and a hidden one, so a non-existence claim is false for half the cases", got, forbidden)
			}
		}
	})

	t.Run("ref without a title does not render a dangling separator", func(t *testing.T) {
		t.Parallel()
		got := RenderRelationValue(models.RelationTarget{ID: "abc", Ref: "COLO-3"})
		if got != "COLO-3" {
			t.Errorf("got %q, want %q", got, "COLO-3")
		}
	})
}
