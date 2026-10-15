package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3814: every item-version door read the item's body, then the version
// list, as two pool reads, and resolved the list's reverse patches from that
// body. A content write landing between the two adds a version (or rebases
// the newest one) against the NEW body while the walk starts from the OLD
// one. diff.ApplyPatch does not check a patch's base, so the walk can produce
// text no version ever held, and restore writes it as canonical.

func bug3814Lines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "Line %d of the plan.\n", i)
	}
	return b.String()
}

var (
	bug3814Target  = bug3814Lines(1, 20)                           // the version the caller asks for
	bug3814Current = bug3814Lines(1, 21)                           // the body when the request starts
	bug3814Newer   = "Line 0 of the plan.\n" + bug3814Lines(1, 21) // the concurrent write
)

type bug3814Fixture struct {
	srv            *Server
	wsSlug, itemID string
	itemSlug       string
}

// One version, "vt": a reverse patch from the current body back to the
// target, created now so a throttled write rebases it.
func newBug3814Fixture(t *testing.T) *bug3814Fixture {
	t.Helper()
	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3814"})
	if err != nil {
		t.Fatal(err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: bug3814Current, Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	db := srv.store.DB()
	if _, err := db.Exec(`DELETE FROM item_versions WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, 1, ?, 1)`,
		"vt", item.ID, diff.CreateReversePatch(bug3814Target, bug3814Current), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return &bug3814Fixture{srv: srv, wsSlug: ws.Slug, itemID: item.ID, itemSlug: item.Slug}
}

// concurrentWrite commits bug3814Newer the first time the version list is
// read, in the window this bug is about. force mints a new version; without
// it the write lands inside the throttle and rebases vt (BUG-3745).
func (f *bug3814Fixture) concurrentWrite(t *testing.T, force bool) (fired *int, restore func()) {
	t.Helper()
	n := 0
	nested := false
	restore = f.srv.store.SetBeforeItemVersionsReadHookForTesting(func(id string) {
		if id != f.itemID || nested || n > 0 {
			return
		}
		n++
		nested = true
		content := bug3814Newer
		if _, err := f.srv.store.UpdateItem(f.itemID, models.ItemUpdate{Content: &content, ForceVersion: force}); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
		nested = false
	})
	return &n, restore
}

func (f *bug3814Fixture) url(rest string) string {
	return "/api/v1/workspaces/" + f.wsSlug + "/items/" + f.itemSlug + "/versions" + rest
}

func TestBUG3814_VersionDoorsResolveFromOneSnapshot(t *testing.T) {
	// Instrument: resolving from the stale body must actually go wrong for
	// both kinds of concurrent write, or no leg below can fail.
	if got, err := diff.ApplyPatch(bug3814Current, diff.CreateReversePatch(bug3814Current, bug3814Newer)); err == nil && got == bug3814Current {
		t.Fatal("instrument: a new version's patch resolves correctly from the stale body")
	}
	if got, err := diff.ApplyPatch(bug3814Current, diff.CreateReversePatch(bug3814Target, bug3814Newer)); err == nil && got == bug3814Target {
		t.Fatal("instrument: a rebased patch resolves correctly from the stale body")
	}

	doors := []struct {
		name string
		// body is the text the door reports (or, for restore, writes) for vt.
		body func(t *testing.T, f *bug3814Fixture) string
	}{
		{"restore", func(t *testing.T, f *bug3814Fixture) string {
			rr := doRequest(f.srv, "POST", f.url("/vt/restore"), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
			}
			got, err := f.srv.store.GetItem(f.itemID)
			if err != nil || got == nil {
				t.Fatalf("GetItem: %v", err)
			}
			return got.Content
		}},
		{"list", func(t *testing.T, f *bug3814Fixture) string {
			rr := doRequest(f.srv, "GET", f.url(""), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
			}
			var vs []models.Version
			if err := json.Unmarshal(rr.Body.Bytes(), &vs); err != nil {
				var wrapped struct {
					Versions []models.Version `json:"versions"`
				}
				if err2 := json.Unmarshal(rr.Body.Bytes(), &wrapped); err2 != nil {
					t.Fatalf("decode list: %v / %v", err, err2)
				}
				vs = wrapped.Versions
			}
			for _, v := range vs {
				if v.ID == "vt" {
					return v.Content
				}
			}
			t.Fatalf("vt missing from the list: %s", rr.Body.String())
			return ""
		}},
		{"get", func(t *testing.T, f *bug3814Fixture) string {
			rr := doRequest(f.srv, "GET", f.url("/vt"), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
			}
			var v models.Version
			if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			return v.Content
		}},
		{"diff", func(t *testing.T, f *bug3814Fixture) string {
			rr := doRequest(f.srv, "GET", f.url("/vt/diff"), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("diff: %d %s", rr.Code, rr.Body.String())
			}
			var d models.ItemVersionDiff
			if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			return d.Before
		}},
	}
	for _, door := range doors {
		for _, force := range []bool{true, false} {
			name := door.name + "/rebased"
			if force {
				name = door.name + "/new_version"
			}
			t.Run(name, func(t *testing.T) {
				f := newBug3814Fixture(t)
				fired, restore := f.concurrentWrite(t, force)
				got := door.body(t, f)
				restore()
				if *fired != 1 {
					t.Fatalf("instrument: the concurrent write ran %d times, want 1", *fired)
				}
				if got != bug3814Target {
					t.Fatalf("vt resolved to %q, want the version it records %q", got, bug3814Target)
				}
			})
		}
	}
}
