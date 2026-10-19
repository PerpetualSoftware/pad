package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3617 slice 1: verification evidence over the router. The store's
// state rules are pinned in internal/store/task3617_verifications_test.go;
// these pin the door: validation, the batch's atomicity, visibility, and the
// possibly_stale hint.

const task3617Body = "Spec.\n\n## Acceptance criteria\n\n- AC-1: lists open items\n- AC-2: hides closed items\n- AC-3: <describe>\n"

type task3617List struct {
	Ref           string                    `json:"ref"`
	Criteria      []string                  `json:"criteria"`
	Verifications []models.ItemVerification `json:"verifications"`
}

func task3617Path(slug string, item models.Item) string {
	// By ref, not slug: a retitle re-slugs the item, and the retry tests
	// retitle it.
	return "/api/v1/workspaces/" + slug + "/items/" + item.Ref + "/verifications"
}

func TestTASK3617_RecordAndRead(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
		task := createItem(t, srv, slug, "tasks", map[string]any{"title": "Did it", "fields": `{"status":"open"}`})
		rev := strings.Repeat("ab", 20)

		rr := doRequest(srv, "POST", task3617Path(slug, spec), map[string]any{
			"code_revision": strings.ToUpper(rev),
			"via":           task.Ref,
			"results": []map[string]any{
				{"criterion": "AC-1", "verdict": "pass", "evidence": "TestList", "evidence_url": "https://ci.example/run/1"},
				{"criterion": "AC-2", "verdict": "fail", "evidence": "closed items still shown"},
			},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("record: %d %s", rr.Code, rr.Body.String())
		}
		var created struct {
			RunID         string                    `json:"run_id"`
			Verifications []models.ItemVerification `json:"verifications"`
		}
		parseJSON(t, rr, &created)
		if created.RunID == "" || len(created.Verifications) != 2 {
			t.Fatalf("created: %+v", created)
		}
		for _, v := range created.Verifications {
			if v.State != models.VerificationCurrent || v.CodeRevision != rev || v.ViaRef != task.Ref || v.RunID != created.RunID {
				t.Fatalf("created row: %+v", v)
			}
		}

		rr = doRequest(srv, "GET", task3617Path(slug, spec), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("read: %d %s", rr.Code, rr.Body.String())
		}
		var got task3617List
		parseJSON(t, rr, &got)
		if strings.Join(got.Criteria, ",") != "AC-1,AC-2" {
			t.Fatalf("criteria must list the real ids, not the placeholder: %v", got.Criteria)
		}
		if len(got.Verifications) != 2 || got.Verifications[0].Verdict != "pass" || got.Verifications[1].Verdict != "fail" {
			t.Fatalf("read: %+v", got.Verifications)
		}

		// The write leaves an activity row naming the run.
		acts, err := srv.store.ListDocumentActivity(spec.ID, models.ActivityListParams{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var saw bool
		for _, a := range acts {
			if a.Action == "verified" && strings.Contains(a.Metadata, created.RunID) {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("no verified activity with the run id: %+v", acts)
		}
	})
}

// Every refusal leaves nothing stored: a batch is all or nothing.
func TestTASK3617_Refusals(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
	dup := createItem(t, srv, slug, "docs", map[string]any{"title": "Dup", "content": "## Acceptance criteria\n\n- AC-1: one\n- AC-1: two\n"})
	// One real criterion and one placeholder under the same id (lead spot
	// mutant at d2fb671, TASK-3617): the id names two lines, so a verdict
	// could not say which it was about.
	halfDup := createItem(t, srv, slug, "docs", map[string]any{"title": "Half dup", "content": "## Acceptance criteria\n\n- AC-1: one\n- AC-1: ...\n"})

	ok := map[string]any{"criterion": "AC-1", "verdict": "pass"}
	for _, tc := range []struct {
		name string
		item models.Item
		body map[string]any
		code string
	}{
		{"placeholder", spec, map[string]any{"results": []any{ok, map[string]any{"criterion": "AC-3", "verdict": "pass"}}}, "unknown_criterion"},
		{"missing criterion", spec, map[string]any{"results": []any{ok, map[string]any{"criterion": "AC-9", "verdict": "pass"}}}, "unknown_criterion"},
		{"duplicate id in the body", dup, map[string]any{"results": []any{ok}}, "unknown_criterion"},
		{"id that is both a criterion and a placeholder", halfDup, map[string]any{"results": []any{ok}}, "unknown_criterion"},
		{"bad verdict", spec, map[string]any{"results": []any{map[string]any{"criterion": "AC-1", "verdict": "yes"}}}, "validation_error"},
		{"repeated criterion", spec, map[string]any{"results": []any{ok, ok}}, "validation_error"},
		{"no results", spec, map[string]any{"results": []any{}}, "validation_error"},
		{"short sha", spec, map[string]any{"code_revision": "abc123", "results": []any{ok}}, "validation_error"},
		{"bad run id", spec, map[string]any{"run_id": "nope", "results": []any{ok}}, "validation_error"},
		{"evidence too long", spec, map[string]any{"results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass", "evidence": strings.Repeat("x", 2049)}}}, "validation_error"},
		{"relative url", spec, map[string]any{"results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass", "evidence_url": "/run/1"}}}, "validation_error"},
		{"unknown via", spec, map[string]any{"via": "TASK-99999", "results": []any{ok}}, "unknown_via"},
	} {
		rr := doRequest(srv, "POST", task3617Path(slug, tc.item), tc.body)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, rr.Code, rr.Body.String(), tc.code)
		}
	}
	for _, it := range []models.Item{spec, dup, halfDup} {
		rows, err := srv.store.ListItemVerifications(it.WorkspaceID, it.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("a refused batch stored %d rows on %s", len(rows), it.Ref)
		}
	}
	// The unknown_criterion answer lists the valid ids so the caller can fix
	// the call in one round trip.
	rr := doRequest(srv, "POST", task3617Path(slug, spec), map[string]any{"results": []any{map[string]any{"criterion": "AC-9", "verdict": "pass"}}})
	if !strings.Contains(rr.Body.String(), `"AC-1"`) || !strings.Contains(rr.Body.String(), `"AC-2"`) {
		t.Fatalf("unknown_criterion must name the valid ids: %s", rr.Body.String())
	}
}

// A guest granted edit on the spec may record and read. A via naming an item
// the guest cannot see gets the same answer as one that does not exist, and a
// via ref recorded by someone else is not disclosed to the guest.
func TestTASK3617_GuestVisibility(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}
		spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
		secret := createItem(t, srv, slug, "tasks", map[string]any{"title": "Not for the guest", "fields": `{"status":"open"}`})
		hidden := createItem(t, srv, slug, "docs", map[string]any{"title": "Hidden spec", "content": task3617Body})

		// Recorded by the instance (pre-setup, so no auth) naming the secret
		// task as via.
		if rr := doRequest(srv, "POST", task3617Path(slug, spec), map[string]any{
			"via": secret.Ref, "results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass"}},
		}); rr.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
		}

		granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-3617@example.com", Name: "Granter", Username: "granter-3617", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-3617@example.com", Name: "Guest", Username: "guest-3617", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateItemGrant(ws.ID, spec.ID, guest.ID, "edit", granter.ID); err != nil {
			t.Fatal(err)
		}
		token, err := srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}

		rr := doAuthedJSON(srv, "GET", task3617Path(slug, spec), nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("guest read of a granted spec: %d %s", rr.Code, rr.Body.String())
		}
		var got task3617List
		parseJSON(t, rr, &got)
		if len(got.Verifications) != 1 {
			t.Fatalf("guest read: %+v", got)
		}
		if got.Verifications[0].ViaRef != "" || strings.Contains(rr.Body.String(), secret.Ref) || strings.Contains(rr.Body.String(), secret.ID) {
			t.Fatalf("the guest was shown an item it cannot see: %s", rr.Body.String())
		}

		// The hidden spec is a 404, exactly as the item itself would be.
		if rr := doAuthedJSON(srv, "GET", task3617Path(slug, hidden), nil, token); rr.Code != http.StatusNotFound {
			t.Fatalf("guest read of an ungranted spec: %d, want 404", rr.Code)
		}

		// via: invisible and missing are one answer.
		invisible := doAuthedJSON(srv, "POST", task3617Path(slug, spec), map[string]any{
			"via": secret.Ref, "results": []any{map[string]any{"criterion": "AC-2", "verdict": "pass"}},
		}, token)
		missing := doAuthedJSON(srv, "POST", task3617Path(slug, spec), map[string]any{
			"via": "TASK-99999", "results": []any{map[string]any{"criterion": "AC-2", "verdict": "pass"}},
		}, token)
		if invisible.Code != http.StatusBadRequest || missing.Code != http.StatusBadRequest ||
			strings.Replace(invisible.Body.String(), secret.Ref, "X", 1) != strings.Replace(missing.Body.String(), "TASK-99999", "X", 1) {
			t.Fatalf("invisible via %d %s\nmissing via %d %s", invisible.Code, invisible.Body.String(), missing.Code, missing.Body.String())
		}

		// And the guest's own write, with no via, lands.
		if rr := doAuthedJSON(srv, "POST", task3617Path(slug, spec), map[string]any{
			"results": []any{map[string]any{"criterion": "AC-2", "verdict": "cant_tell"}},
		}, token); rr.Code != http.StatusCreated {
			t.Fatalf("guest write with edit grant: %d %s", rr.Code, rr.Body.String())
		}
	})
}

// A linked PR that merged after a verdict was recorded, at a different SHA,
// marks the verdict possibly_stale and leaves its state alone. A PR whose
// merge commit IS the verdict's revision does not, and nor does a verdict
// with no revision.
func TestTASK3617_PossiblyStaleFromAMergedPR(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
	task := createItem(t, srv, slug, "tasks", map[string]any{"title": "Did it", "fields": `{"status":"open"}`})
	rev := strings.Repeat("1", 40)
	merge := strings.Repeat("2", 40)

	if rr := doRequest(srv, "POST", task3617Path(slug, spec), map[string]any{
		"code_revision": rev, "via": task.Ref,
		"results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass"}},
	}); rr.Code != http.StatusCreated {
		t.Fatalf("record at rev: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequest(srv, "POST", task3617Path(slug, spec), map[string]any{
		"results": []any{map[string]any{"criterion": "AC-2", "verdict": "pass"}},
	}); rr.Code != http.StatusCreated {
		t.Fatalf("record without rev: %d %s", rr.Code, rr.Body.String())
	}

	link := func(sha string, mergedAt time.Time) {
		t.Helper()
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+task.Slug, map[string]any{
			"github_pr": map[string]any{
				"number": 7, "url": "https://github.com/acme/app/pull/7", "state": "MERGED",
				"merge_sha": sha, "merged_at": mergedAt.UTC().Format(time.RFC3339),
			},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("link PR: %d %s", rr.Code, rr.Body.String())
		}
	}
	read := func() map[string]models.ItemVerification {
		t.Helper()
		rr := doRequest(srv, "GET", task3617Path(slug, spec), nil)
		var got task3617List
		parseJSON(t, rr, &got)
		out := map[string]models.ItemVerification{}
		for _, v := range got.Verifications {
			out[v.CriterionID] = v
		}
		return out
	}

	later := time.Now().Add(time.Hour)
	link(merge, later)
	got := read()
	h := got["AC-1"].PossiblyStale
	if h == nil || h.Via != "PR #7" || h.MergedSHA != merge {
		t.Fatalf("AC-1 must carry the hint naming PR #7 at %s: %+v", merge, got["AC-1"])
	}
	if got["AC-1"].State != models.VerificationCurrent {
		t.Fatalf("the hint must not change state: %s", got["AC-1"].State)
	}
	if got["AC-2"].PossiblyStale != nil {
		t.Fatalf("a verdict with no revision is not about code: %+v", got["AC-2"])
	}

	link(rev, later)
	if got = read(); got["AC-1"].PossiblyStale != nil {
		t.Fatalf("a PR that merged AT the verdict's revision did not move it: %+v", got["AC-1"].PossiblyStale)
	}

	// A PR that merged BEFORE the verdict was recorded is code the verifier
	// already had, whatever its SHA.
	link(merge, time.Now().Add(-time.Hour))
	if got = read(); got["AC-1"].PossiblyStale != nil {
		t.Fatalf("a PR merged before the verdict did not move it: %+v", got["AC-1"].PossiblyStale)
	}

	// A malformed merge_sha is refused at the PR door, so the hint can only
	// ever quote a real SHA.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+task.Slug, map[string]any{
		"github_pr": map[string]any{"number": 7, "url": "https://github.com/acme/app/pull/7", "merge_sha": "<script>"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed merge_sha: %d %s, want 400", rr.Code, rr.Body.String())
	}
}

// TASK-3617 r1: the retry when the item moves between the criteria read and
// the write. One move retries and lands, against the new seq. An item that
// keeps moving is a 409. A move that also takes the item out of the caller's
// reach is refused on the retry, not written: the first read's checks
// authorized only that read.
func TestTASK3617_RetryOnAMovedItem(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
	body := map[string]any{"results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass"}}}

	bump := func(t *testing.T, it models.Item) {
		t.Helper()
		title := "Spec " + time.Now().Format(time.RFC3339Nano)
		if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}

	var attempts []int
	srv.verificationBeforeInsertHook = func(attempt int) {
		attempts = append(attempts, attempt)
		if attempt == 0 {
			bump(t, spec)
		}
	}
	rr := doRequest(srv, "POST", task3617Path(slug, spec), body)
	if rr.Code != http.StatusCreated || len(attempts) != 2 {
		t.Fatalf("one move: %d after %v attempts, want 201 after [0 1]: %s", rr.Code, attempts, rr.Body.String())
	}
	fresh, err := srv.store.GetItem(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := srv.store.ListItemVerifications(fresh.WorkspaceID, spec.ID, false)
	if err != nil || len(rows) != 1 || rows[0].SpecSeq != fresh.Seq {
		t.Fatalf("the retried row must carry the seq it was written against (%d): %+v %v", fresh.Seq, rows, err)
	}

	srv.verificationBeforeInsertHook = func(int) { bump(t, spec) }
	if rr := doRequest(srv, "POST", task3617Path(slug, spec), body); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "item_changed") {
		t.Fatalf("an item that keeps moving: %d %s, want 409 item_changed", rr.Code, rr.Body.String())
	}
	srv.verificationBeforeInsertHook = nil
	if rows, _ := srv.store.ListItemVerifications(fresh.WorkspaceID, spec.ID, false); len(rows) != 1 {
		t.Fatalf("the 409 must store nothing: %d rows", len(rows))
	}
}

func TestTASK3617_RetryRechecksAccess(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}
		spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
		granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-3617r@example.com", Name: "Granter", Username: "granter-3617r", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-3617r@example.com", Name: "Guest", Username: "guest-3617r", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		grant, err := srv.store.CreateItemGrant(ws.ID, spec.ID, guest.ID, "edit", granter.ID)
		if err != nil {
			t.Fatal(err)
		}
		token, err := srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}

		// Between the read and the write, the grant is revoked and the item
		// moves on.
		srv.verificationBeforeInsertHook = func(attempt int) {
			if attempt != 0 {
				return
			}
			if err := srv.store.DeleteItemGrant(grant.ID, ws.ID); err != nil {
				t.Error(err)
			}
			title := "Spec, revised"
			if _, err := srv.store.UpdateItem(spec.ID, models.ItemUpdate{Title: &title}); err != nil {
				t.Error(err)
			}
		}
		rr := doAuthedJSON(srv, "POST", task3617Path(slug, spec), map[string]any{
			"results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass"}},
		}, token)
		srv.verificationBeforeInsertHook = nil
		if rr.Code != http.StatusNotFound {
			t.Fatalf("a retry after the grant was revoked: %d %s, want 404", rr.Code, rr.Body.String())
		}
		if rows, _ := srv.store.ListItemVerifications(ws.ID, spec.ID, false); len(rows) != 0 {
			t.Fatalf("the refused retry stored %d rows", len(rows))
		}
	})
}

// TASK-3617 r2 L1: the same re-check for via. The guest can see the task on
// the first read; between that read and the write the task's grant is
// revoked and the spec moves. The retry must resolve via again and refuse,
// not store a reference to an item the caller can no longer see.
func TestTASK3617_RetryRechecksVia(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}
		spec := createItem(t, srv, slug, "docs", map[string]any{"title": "Spec", "content": task3617Body})
		task := createItem(t, srv, slug, "tasks", map[string]any{"title": "Did it", "fields": `{"status":"open"}`})
		granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-3617v@example.com", Name: "Granter", Username: "granter-3617v", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-3617v@example.com", Name: "Guest", Username: "guest-3617v", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateItemGrant(ws.ID, spec.ID, guest.ID, "edit", granter.ID); err != nil {
			t.Fatal(err)
		}
		taskGrant, err := srv.store.CreateItemGrant(ws.ID, task.ID, guest.ID, "view", granter.ID)
		if err != nil {
			t.Fatal(err)
		}
		token, err := srv.store.CreateSession(guest.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}

		srv.verificationBeforeInsertHook = func(attempt int) {
			if attempt != 0 {
				return
			}
			if err := srv.store.DeleteItemGrant(taskGrant.ID, ws.ID); err != nil {
				t.Error(err)
			}
			title := "Spec, revised"
			if _, err := srv.store.UpdateItem(spec.ID, models.ItemUpdate{Title: &title}); err != nil {
				t.Error(err)
			}
		}
		rr := doAuthedJSON(srv, "POST", task3617Path(slug, spec), map[string]any{
			"via": task.Ref, "results": []any{map[string]any{"criterion": "AC-1", "verdict": "pass"}},
		}, token)
		srv.verificationBeforeInsertHook = nil
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown_via") {
			t.Fatalf("a retry after the via grant was revoked: %d %s, want 400 unknown_via", rr.Code, rr.Body.String())
		}
		if rows, _ := srv.store.ListItemVerifications(ws.ID, spec.ID, false); len(rows) != 0 {
			t.Fatalf("the refused retry stored %d rows", len(rows))
		}
	})
}
