package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Retro CONVE-735 finding on #2070: the keyed replay of an archived-source
// move re-checked the source but not the destination, so a caller who had
// since lost the destination could read the stored copy result. The replay
// now runs the ladder's destination checks; a caller without the destination
// gets the normal refusal, never the stored answer.
func TestTASK3585_ArchivedSourceReplayRechecksTheDestination(t *testing.T) {
	t.Parallel()
	f := newCopyPreflightFixture(t)
	u := f.restrictedEditor("mover@example.com", "mover", []string{f.collA.ID}, []string{f.collB.ID})
	o := reqOpts{wsRoleCtx: "editor"}
	move := f.resolvableBody()
	move["archive_source"] = true
	move["op_key"] = "mv-authz"

	first := f.callCopy(u, o, move)
	if first.Code != http.StatusCreated {
		t.Fatalf("move: %d %s", first.Code, first.Body.String())
	}
	var res ItemCopyResult
	_ = json.Unmarshal(first.Body.Bytes(), &res)
	if res.Destination.Ref == "" {
		t.Fatalf("move answer names no destination: %s", first.Body.String())
	}

	// Control: with the destination still allowed, the retry replays.
	if again := f.callCopy(u, o, move); again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay while allowed: %d %s", again.Code, again.Body.String())
	}

	// The mover loses the destination collection.
	other := mustSchemaCollection(t, f.srv, f.wsB.ID, "Other B", dstSchemaJSON)
	if err := f.srv.store.SetMemberCollectionAccess(f.wsB.ID, u.ID, "specific", []string{other.ID}); err != nil {
		t.Fatal(err)
	}
	rr := f.callCopy(u, o, move)
	if rr.Code == http.StatusCreated || rr.Code < 400 {
		t.Fatalf("a caller who lost the destination got the stored answer: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), res.Destination.Ref) || rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("the refusal leaked the stored result: %v %s", rr.Header(), rr.Body.String())
	}
}
