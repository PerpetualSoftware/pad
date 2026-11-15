package server

import (
	"strings"
	"testing"
	"time"
)

func TestPastDeadlineHTTPContract(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	b := commitmentBody()
	b["dtstart"], b["tz"] = start.Format("2006-01-02T15:04"), "UTC"
	readCommitment(t, commitmentCall(f, "POST", b), 201)
	deadline := start.Add(24 * time.Hour).Format(time.RFC3339)
	token := f.token(f.owner, "")
	p := commitmentPath(f)
	body := map[string]any{"occurrence_at": start.Format(time.RFC3339), "state": "move", "until": start.Add(4 * 24 * time.Hour).Format(time.RFC3339)}
	before := commitmentSnapshot(t, f)
	rr := f.as(token, "POST", p+"/resolve", body)
	f.expectErr(rr, 409, "target_after_deadline", "")
	e := schedMap(t, rr)["error"].(map[string]any)
	if e["details"].(map[string]any)["deadline"] != deadline {
		t.Fatalf("error=%v", e)
	}
	if before != commitmentSnapshot(t, f) {
		t.Fatal("refusal wrote")
	}
	body["confirm_past_deadline"] = "yes"
	f.expectErr(f.as(token, "POST", p+"/resolve", body), 400, "invalid_body", "")
	body["confirm_past_deadline"] = true
	body["state"] = "skip"
	delete(body, "until")
	rr = f.as(token, "POST", p+"/resolve", body)
	f.expectErr(rr, 400, "validation_error", "combination")
	body["state"], body["until"] = "move", start.Add(4*24*time.Hour).Format(time.RFC3339)
	rr = f.as(token, "POST", p+"/resolve", body)
	if rr.Code != 200 || schedMap(t, rr)["resolution"].(map[string]any)["warning"] != "target_after_deadline" {
		t.Fatalf("confirmed: %d %s", rr.Code, rr.Body.String())
	}
	move := map[string]any{"role": "target", "to": start.Add(5 * 24 * time.Hour).Format(time.RFC3339)}
	rr = f.as(token, "POST", p+"/move-preview", move)
	if rr.Code != 200 {
		t.Fatalf("preview %d %s", rr.Code, rr.Body.String())
	}
	preview := schedMap(t, rr)
	if preview["past_deadline"] != deadline {
		t.Fatalf("preview=%v", preview)
	}
	move["revision"] = preview["revision"]
	f.expectErr(f.as(token, "POST", p+"/move", move), 409, "target_after_deadline", "")
	move["confirm_past_deadline"] = "yes"
	f.expectErr(f.as(token, "POST", p+"/move", move), 400, "invalid_body", "")
	move["confirm_past_deadline"] = true
	f.expectErr(f.as(token, "POST", p+"/move-preview", map[string]any{"role": "target", "to": move["to"], "confirm_past_deadline": true}), 400, "invalid_body", "")
	rr = f.as(token, "POST", p+"/move", move)
	if rr.Code != 200 || schedMap(t, rr)["past_deadline"] != deadline {
		t.Fatalf("move %d %s", rr.Code, rr.Body.String())
	}
}

func TestPastDeadlineHTTPVisibilityAndEditGates(t *testing.T) {
	t.Parallel()
	f := newSchedHTTP(t)
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	viewer := f.member("deadline-viewer@example.com", "viewer")
	token := f.token(viewer, "")
	for _, door := range []string{"resolve", "move"} {
		body := map[string]any{"state": "move", "occurrence_at": "2099-01-01T09:00:00Z", "until": "2099-01-05T09:00:00Z"}
		if door == "move" {
			body = map[string]any{"role": "target", "to": "2099-01-05T09:00:00Z"}
		}
		rr := f.as(token, "POST", commitmentPath(f)+"/"+door, body)
		if rr.Code != 403 || strings.Contains(rr.Body.String(), "target_after_deadline") {
			t.Fatalf("%s viewer: %d %s", door, rr.Code, rr.Body.String())
		}
	}
	allowed := mustCollection(t, f.srv, f.ws.ID, "Allowed")
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, viewer.ID, "specific", []string{allowed.ID}); err != nil {
		t.Fatal(err)
	}
	for _, door := range []string{"resolve", "move"} {
		body := map[string]any{"state": "move", "occurrence_at": "2099-01-01T09:00:00Z", "until": "2099-01-05T09:00:00Z"}
		if door == "move" {
			body = map[string]any{"role": "target", "to": "2099-01-05T09:00:00Z"}
		}
		path := commitmentPath(f) + "/" + door
		hidden := f.as(token, "POST", path, body)
		missing := f.as(token, "POST", strings.Replace(path, "/items/"+f.item.Slug+"/", "/items/no-such-item/", 1), body)
		if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() {
			t.Fatalf("%s hidden=%d %s missing=%d %s", door, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
	}

}
