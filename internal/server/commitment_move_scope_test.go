package server

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The move scope: occurrence (the default) or series, which also writes the
// root's native target offset from the outstanding occurrence on.

func seriesMoveFixture(t *testing.T) *schedHTTP {
	t.Helper()
	f := newSchedHTTP(t)
	b := commitmentBody()
	b["rrule"] = "FREQ=DAILY"
	readCommitment(t, commitmentCall(f, "POST", b), 201)
	return f
}

func TestMoveScopeValidation(t *testing.T) {
	t.Parallel()
	f := seriesMoveFixture(t)
	tok := f.token(f.owner, "")
	p := commitmentPath(f)
	to := "2099-01-01T14:00:00Z"
	before := commitmentSnapshot(t, f)
	for _, door := range []string{"/move-preview", "/move"} {
		f.expectErr(f.as(tok, "POST", p+door, map[string]any{"role": "target", "to": to, "scope": "bogus"}), 400, "validation_error", "enum")
		d := f.expectErr(f.as(tok, "POST", p+door, map[string]any{"role": "target", "to": to, "scope": ""}), 400, "validation_error", "enum")
		if d["field"] != "scope" {
			t.Fatalf("details=%v", d)
		}
		d = f.expectErr(f.as(tok, "POST", p+door, map[string]any{"role": "earliest", "to": to, "scope": "series"}), 400, "validation_error", "combination")
		if d["field"] != "scope" {
			t.Fatalf("details=%v", d)
		}
		f.expectErr(f.as(tok, "POST", p+door, map[string]any{"role": "target", "to": to, "scope": 5}), 400, "invalid_body", "")
	}
	d := f.expectErr(f.as(tok, "POST", p+"/move", map[string]any{"role": "target", "to": to, "scope": "series"}), 400, "validation_error", "required")
	if d["field"] != "revision" {
		t.Fatalf("details=%v", d)
	}
	d = f.expectErr(f.as(tok, "POST", p+"/move-preview", map[string]any{"role": "target", "to": "2099-01-02T10:00:00Z", "scope": "series"}), 400, "validation_error", "range")
	if d["field"] != "to" {
		t.Fatalf("details=%v", d)
	}
	if before != commitmentSnapshot(t, f) {
		t.Fatal("a refused move wrote")
	}
}

func TestMoveScopeDefaultBytesUnchanged(t *testing.T) {
	t.Parallel()
	f := seriesMoveFixture(t)
	tok := f.token(f.owner, "")
	p := commitmentPath(f)
	body := map[string]any{"role": "target", "to": "2099-01-01T14:00:00Z"}
	plain := f.as(tok, "POST", p+"/move-preview", body)
	body["scope"] = "occurrence"
	explicit := f.as(tok, "POST", p+"/move-preview", body)
	if plain.Code != 200 || plain.Body.String() != explicit.Body.String() || strings.Contains(plain.Body.String(), "series") {
		t.Fatalf("plain=%d %s explicit=%s", plain.Code, plain.Body.String(), explicit.Body.String())
	}
	delete(body, "scope")
	applied := f.as(tok, "POST", p+"/move", body)
	if applied.Code != 200 || strings.Contains(applied.Body.String(), "series") {
		t.Fatalf("apply=%d %s", applied.Code, applied.Body.String())
	}
}

func TestMoveScopeSeriesPreviewShape(t *testing.T) {
	t.Parallel()
	f := seriesMoveFixture(t)
	tok := f.token(f.owner, "")
	p := commitmentPath(f)
	body := map[string]any{"role": "target", "to": "2099-01-01T14:00:00Z", "scope": "series"}
	rr := f.as(tok, "POST", p+"/move-preview", body)
	if rr.Code != 200 {
		t.Fatalf("preview %d %s", rr.Code, rr.Body.String())
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	var series map[string]any
	if err := json.Unmarshal(shape["series"], &series); err != nil {
		t.Fatalf("series: %s", rr.Body.String())
	}
	keys := []string{}
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := map[string]any{"applies_from": "2099-01-01T09:00:00Z|2099-01-01T09:00:00",
		"current_offset": map[string]any{"target_days": float64(0), "target_secs": float64(0)},
		"new_offset":     map[string]any{"target_days": float64(0), "target_secs": float64(18000)}}
	if !reflect.DeepEqual(keys, []string{"applies_from", "current_offset", "new_offset"}) || !reflect.DeepEqual(series, want) {
		t.Fatalf("series=%v", series)
	}
	var preview models.LinkedMoveResult
	_ = json.Unmarshal(rr.Body.Bytes(), &preview)
	body["revision"] = preview.Revision
	rr = f.as(tok, "POST", p+"/move", body)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"series":{"applies_from"`) {
		t.Fatalf("apply %d %s", rr.Code, rr.Body.String())
	}
	c := readCommitment(t, commitmentCall(f, "GET", nil), 200)
	if c.Roles.Target == nil || *c.Roles.Target != "2099-01-01T14:00:00Z" {
		t.Fatalf("target=%v", c.Roles.Target)
	}
}

func TestMoveScopeHidden404Identical(t *testing.T) {
	t.Parallel()
	f := seriesMoveFixture(t)
	viewer := f.member("series-hidden@example.com", "editor")
	allowed := mustCollection(t, f.srv, f.ws.ID, "Allowed")
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, viewer.ID, "specific", []string{allowed.ID}); err != nil {
		t.Fatal(err)
	}
	tok := f.token(viewer, "")
	for _, door := range []string{"/move-preview", "/move"} {
		for _, scope := range []any{"series", "bogus", 5} {
			body := map[string]any{"role": "earliest", "to": "2099-01-01T14:00:00Z", "scope": scope}
			path := commitmentPath(f) + door
			hidden := f.as(tok, "POST", path, body)
			missing := f.as(tok, "POST", strings.Replace(path, "/items/"+f.item.Slug+"/", "/items/no-such-item/", 1), body)
			if hidden.Code != 404 || hidden.Body.String() != missing.Body.String() {
				t.Fatalf("%s %v hidden=%d %s missing=%d %s", door, scope, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
			}
		}
	}
}

func TestMoveScopeViewer403(t *testing.T) {
	t.Parallel()
	f := seriesMoveFixture(t)
	tok := f.token(f.member("series-viewer@example.com", "viewer"), "")
	before := commitmentSnapshot(t, f)
	for _, door := range []string{"/move-preview", "/move"} {
		body := map[string]any{"role": "target", "to": "2099-01-01T14:00:00Z", "scope": "series"}
		if door == "/move" {
			body["revision"] = strings.Repeat("a", 43)
		}
		rr := f.as(tok, "POST", commitmentPath(f)+door, body)
		if rr.Code != 403 {
			t.Fatalf("%s viewer: %d %s", door, rr.Code, rr.Body.String())
		}
	}
	if before != commitmentSnapshot(t, f) {
		t.Fatal("a refused move wrote")
	}
}

func TestMoveScopeHiddenPartnerMessage(t *testing.T) {
	t.Parallel()
	f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
	f.link(0, 1, "target", "target", "")
	tok := f.token(f.guest(0), "")
	start, _ := time.Parse(time.RFC3339, f.dates[0])
	body := map[string]any{"role": "target", "to": start.Add(5 * time.Hour).Format(time.RFC3339), "scope": "series"}
	rr := f.as(tok, "POST", f.path(f.item)+"/move-preview", body)
	f.expectErr(rr, 409, "schedule_linked", "")
	if !strings.Contains(rr.Body.String(), hiddenCommitmentPartnerMessage) || strings.Contains(rr.Body.String(), f.items[1].ID) {
		t.Fatalf("refusal=%s", rr.Body.String())
	}
	// The occurrence move of the same date is still allowed.
	delete(body, "scope")
	if rr := f.as(tok, "POST", f.path(f.item)+"/move-preview", body); rr.Code != 200 {
		t.Fatalf("occurrence preview %d %s", rr.Code, rr.Body.String())
	}
}
