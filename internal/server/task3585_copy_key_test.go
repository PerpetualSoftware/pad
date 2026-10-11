package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3585: idempotency keys on cross-workspace copy and move.
func countInColl(t *testing.T, f *copyPreflightFixture) int {
	t.Helper()
	items, err := f.srv.store.ListItems(f.wsB.ID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.CollectionID == f.collB.ID {
			n++
		}
	}
	return n
}

func TestTASK3585_CopyReplaysTheFirstCopy(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()
	body["op_key"] = "cp-1"
	first := f.callCopy(f.owner, reqOpts{}, body)
	again := f.callCopy(f.owner, reqOpts{}, body)
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d/%d %s", first.Code, again.Code, again.Body.String())
	}
	var a, b ItemCopyResult
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if a.Destination.Ref == "" || a.Destination.Ref != b.Destination.Ref {
		t.Fatalf("the replay must name the first copy: %q vs %q", a.Destination.Ref, b.Destination.Ref)
	}
	if n := countInColl(t, f); n != 1 {
		t.Fatalf("%d items in the destination; the retry copied again", n)
	}
	other := f.resolvableBody()
	other["field_overrides"] = map[string]any{"ticket": "T-2", "code": "123"}
	other["op_key"] = "cp-1"
	if rr := f.callCopy(f.owner, reqOpts{}, other); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key: %d %s", rr.Code, rr.Body.String())
	}
}

// A move archives its source, so a retry must replay rather than meet the
// archived source as a refusal or a second copy.
func TestTASK3585_MoveReplaysAfterTheSourceIsArchived(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()
	body["archive_source"] = true
	body["op_key"] = "mv-1"
	first := f.callCopy(f.owner, reqOpts{}, body)
	again := f.callCopy(f.owner, reqOpts{}, body)
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("move replay: %d/%d %s", first.Code, again.Code, again.Body.String())
	}
	if n := countInColl(t, f); n != 1 {
		t.Fatalf("%d items in the destination after a retried move", n)
	}
}

func TestTASK3585_RefusedCopyReleasesItsKey(t *testing.T) {
	f := newCopyPreflightFixture(t)
	bad := f.baseBody() // ticket is required with no default: refused
	bad["op_key"] = "cp-2"
	if rr := f.callCopy(f.owner, reqOpts{}, bad); rr.Code < 400 {
		t.Fatalf("premise: the incomplete copy must be refused; got %d", rr.Code)
	}
	good := f.resolvableBody()
	good["op_key"] = "cp-2"
	rr := f.callCopy(f.owner, reqOpts{}, good)
	if rr.Code != http.StatusCreated || rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected retry with the same key: %d %s", rr.Code, rr.Body.String())
	}
}

// The archived-source path answers only what the key settles: a key that
// never copied this item gets the ladder's own 409 archived, and a different
// request under a used key is 422.
func TestTASK3585_ArchivedSourcePathRefusesWhatTheKeyDoesNotSettle(t *testing.T) {
	f := newCopyPreflightFixture(t)
	move := f.resolvableBody()
	move["archive_source"] = true
	move["op_key"] = "mv-2"
	if rr := f.callCopy(f.owner, reqOpts{}, move); rr.Code != http.StatusCreated {
		t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
	}
	fresh := f.resolvableBody()
	fresh["op_key"] = "never-used"
	if rr := f.callCopy(f.owner, reqOpts{}, fresh); rr.Code != http.StatusConflict || errCode(t, rr) != "archived" {
		t.Fatalf("an unused key on an archived source: %d %s", rr.Code, rr.Body.String())
	}
	other := f.resolvableBody()
	other["archive_source"] = true
	other["field_overrides"] = map[string]any{"ticket": "T-9", "code": "123"}
	other["op_key"] = "mv-2"
	if rr := f.callCopy(f.owner, reqOpts{}, other); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a different request under the used key: %d %s", rr.Code, rr.Body.String())
	}
	if n := countInColl(t, f); n != 1 {
		t.Fatalf("%d items in the destination", n)
	}
}
