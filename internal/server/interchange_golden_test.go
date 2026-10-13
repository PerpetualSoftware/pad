package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3628 (PLAN-3623 Phase 0): a golden bundle that carries every entity the
// interchange format holds, imported and exported again, and diffed against
// itself after ids are mapped back.
//
// testdata/interchange/golden is the bundle laid out as files: pad-export.json,
// attachments/manifest.json and the blobs. The test packs it into a tar.gz,
// imports it through the HTTP bundle door, exports the result through the same
// door, and compares the two. Every difference must match an entry in
// interchangeKnownLosses, which says why it is expected; an entry that matches
// nothing is stale and fails too. So the diff pins today's behaviour and any
// drift in either direction fails.
//
// Regenerate the golden (a deliberate act: it is the INPUT, so regenerating
// re-baselines what the format is checked against) with
//
//	PAD_UPDATE_INTERCHANGE_GOLDEN=1 go test ./internal/server/ -run TestInterchangeGolden
const interchangeGoldenDir = "testdata/interchange/golden"

// interchangeKnownLosses lists every expected difference between the golden
// bundle and its re-export, as a pattern over the flattened diff path and the
// reason. Paths are `<section>/<entity key>/<field>[/<nested key>...]`.
var interchangeKnownLosses = []struct {
	path   string
	reason string
}{
	{`^attachments/attachment:[0-9a-f]{64}/created_at$`,
		"import writes a new attachment row stamped now; the manifest's created_at is not restored"},
	{`^item_links/link:item:Parent task><unknown [0-9a-f-]{36}>\|blocks/`,
		"a link whose endpoint is a soft-deleted item is exported (the raw graph) but skipped on import, because the item is not in the bundle (lead ruling on TASK-3628, day 92)"},
}

func TestInterchangeGolden_RoundTrip(t *testing.T) {
	if os.Getenv("PAD_UPDATE_INTERCHANGE_GOLDEN") != "" {
		writeInterchangeGolden(t)
	}
	golden := readInterchangeGolden(t)

	dest, _ := testServerWithAttachments(t)
	rr := postBundle(dest, "", packInterchangeBundle(t, golden))
	if rr.Code != http.StatusCreated {
		t.Fatalf("import golden: %d %s", rr.Code, rr.Body.String())
	}
	var imported struct {
		Slug string `json:"slug"`
	}
	parseJSON(t, rr, &imported)
	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+imported.Slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("re-export: %d %s", rr.Code, rr.Body.String())
	}
	again := readBundle(t, rr.Body.Bytes())

	want := flattenInterchange(t, "golden", golden)
	got := flattenInterchange(t, "re-export", again)

	var diffs []string
	for p, w := range want {
		if g, ok := got[p]; !ok {
			diffs = append(diffs, p+"  (only in golden: "+w+")")
		} else if g != w {
			diffs = append(diffs, p+"  (golden "+w+", re-export "+g+")")
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok {
			diffs = append(diffs, p+"  (only in re-export: "+g+")")
		}
	}
	sort.Strings(diffs)

	used := make([]bool, len(interchangeKnownLosses))
	var unexpected []string
	for _, d := range diffs {
		path := d[:strings.Index(d, "  (")]
		matched := false
		for i, l := range interchangeKnownLosses {
			if regexp.MustCompile(l.path).MatchString(path) {
				used[i], matched = true, true
			}
		}
		if !matched {
			unexpected = append(unexpected, d)
		}
	}
	for _, d := range unexpected {
		t.Errorf("round-trip difference not in interchangeKnownLosses: %s", d)
	}
	for i, l := range interchangeKnownLosses {
		if !used[i] {
			t.Errorf("stale interchangeKnownLosses entry %q (%s): no difference matches it any more", l.path, l.reason)
		}
	}
}

// readInterchangeGolden loads the golden directory as bundle entries.
func readInterchangeGolden(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(interchangeGoldenDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(interchangeGoldenDir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatalf("read golden bundle: %v", err)
	}
	if out["pad-export.json"] == nil || out["attachments/manifest.json"] == nil {
		t.Fatalf("golden bundle in %s is incomplete: %d entries", interchangeGoldenDir, len(out))
	}
	return out
}

// packInterchangeBundle writes the entries as a tar.gz in the exporter's order:
// pad-export.json, the manifest, then the blobs by name.
func packInterchangeBundle(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := []string{"pad-export.json", "attachments/manifest.json"}
	var blobs []string
	for n := range files {
		if n != names[0] && n != names[1] {
			blobs = append(blobs, n)
		}
	}
	sort.Strings(blobs)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range append(names, blobs...) {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var interchangeUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// flattenInterchange turns a bundle into path -> value, with every entity keyed
// by a natural key instead of its id (an import mints new ids), every id
// anywhere in the bundle rewritten to its entity's key, JSON-in-a-string
// fields parsed so key order cannot differ, and each blob reduced to its
// sha256.
func flattenInterchange(t *testing.T, side string, files map[string][]byte) map[string]string {
	t.Helper()
	var exp map[string]any
	if err := json.Unmarshal(files["pad-export.json"], &exp); err != nil {
		t.Fatalf("%s: decode pad-export.json: %v", side, err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(files["attachments/manifest.json"], &manifest); err != nil {
		t.Fatalf("%s: decode manifest: %v", side, err)
	}

	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	list := func(m map[string]any, k string) []map[string]any {
		raw, _ := m[k].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, r := range raw {
			out = append(out, r.(map[string]any))
		}
		return out
	}

	// Natural keys, built in dependency order so a key can name its parent's.
	ids := map[string]string{}
	key := func(id, k string) {
		if prev, ok := ids[id]; ok && prev != k {
			t.Fatalf("%s: id %s keyed twice (%s, %s)", side, id, prev, k)
		}
		ids[id] = k
	}
	for _, c := range list(exp, "collections") {
		key(str(c, "id"), "col:"+str(c, "slug"))
	}
	for _, it := range list(exp, "items") {
		key(str(it, "id"), "item:"+str(it, "title"))
	}
	ref := func(id string) string {
		if k, ok := ids[id]; ok {
			return k
		}
		return "<unknown " + id + ">"
	}
	for _, c := range list(exp, "comments") {
		key(str(c, "id"), "comment:"+ref(str(c, "item_id"))+"|"+str(c, "body"))
	}
	for _, l := range list(exp, "item_links") {
		key(str(l, "id"), "link:"+ref(str(l, "source_id"))+">"+ref(str(l, "target_id"))+"|"+str(l, "link_type"))
	}
	for _, v := range list(exp, "item_versions") {
		h := sha256.Sum256([]byte(str(v, "content")))
		key(str(v, "id"), "version:"+ref(str(v, "item_id"))+"|"+str(v, "created_at")+"|"+hex.EncodeToString(h[:6]))
	}
	for _, a := range list(manifest, "entries") {
		key(str(a, "id"), "attachment:"+str(a, "content_hash"))
	}

	// Rewrite every id, wherever it appears, to its key.
	var rewrite func(v any) any
	rewrite = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				x[k] = rewrite(e)
			}
		case []any:
			for i, e := range x {
				x[i] = rewrite(e)
			}
		case string:
			return interchangeUUID.ReplaceAllStringFunc(x, ref)
		}
		return v
	}
	rewrite(exp)
	rewrite(manifest)

	out := map[string]string{}
	var flat func(prefix string, v any)
	flat = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				flat(prefix+"/"+k, e)
			}
		case []any:
			for i, e := range x {
				flat(fmt.Sprintf("%s/%d", prefix, i), e)
			}
		case string:
			if s := strings.TrimSpace(x); (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) && json.Valid([]byte(s)) {
				var parsed any
				_ = json.Unmarshal([]byte(s), &parsed)
				flat(prefix+"/(json)", parsed)
				return
			}
			out[strings.TrimPrefix(prefix, "/")] = fmt.Sprintf("%q", x)
		default:
			b, _ := json.Marshal(x)
			out[strings.TrimPrefix(prefix, "/")] = string(b)
		}
	}
	// Entity sections are keyed by natural key, not position.
	keyed := func(section string, rows []map[string]any, k func(map[string]any) string) {
		seen := map[string]bool{}
		for _, r := range rows {
			rk := k(r)
			if seen[rk] {
				t.Fatalf("%s: two %s rows share the key %s", side, section, rk)
			}
			seen[rk] = true
			delete(r, "id")
			flat(section+"/"+rk, r)
		}
	}
	byID := func(r map[string]any) string { return str(r, "id") }
	keyed("collections", list(exp, "collections"), byID)
	keyed("items", list(exp, "items"), byID)
	keyed("comments", list(exp, "comments"), byID)
	keyed("item_links", list(exp, "item_links"), byID)
	keyed("item_versions", list(exp, "item_versions"), byID)
	keyed("reminders", list(exp, "reminders"), func(r map[string]any) string { return str(r, "item_id") + "@" + str(r, "remind_at") })
	keyed("attachments", list(manifest, "entries"), byID)
	// Stamped at export time, so it differs whenever the two exports fall in
	// different seconds.
	delete(exp, "exported_at")
	for _, k := range []string{"collections", "items", "comments", "item_links", "item_versions", "reminders"} {
		delete(exp, k)
	}
	flat("", exp)
	if v, ok := manifest["version"]; ok {
		flat("manifest_version", v)
	}

	// Blobs, by the content they hold.
	for name, b := range files {
		if !strings.HasPrefix(name, "attachments/") || name == "attachments/manifest.json" {
			continue
		}
		h := sha256.Sum256(b)
		out["blobs/"+ref(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))] = hex.EncodeToString(h[:])
	}
	return out
}

// writeInterchangeGolden builds a workspace that holds every entity the format
// carries, exports it as a bundle and writes the bundle into the golden
// directory, replacing what was there.
func writeInterchangeGolden(t *testing.T) {
	t.Helper()
	srv, _ := testServerWithAttachments(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Interchange Golden", "template": "blank"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	base := "/api/v1/workspaces/" + ws.Slug
	post := func(path string, body any) map[string]any {
		t.Helper()
		rr := doRequest(srv, "POST", base+path, body)
		if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, rr.Code, rr.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return out
	}
	st := srv.store

	// Collections and schemas: select with terminal and abandoned options, a
	// default, a number, a date, and a relation to the same collection.
	post("/collections", map[string]any{
		"name": "Tasks", "slug": "tasks", "prefix": "TASK", "icon": "✅", "description": "Work to do",
		"schema": `{"fields":[` +
			`{"key":"status","label":"Status","type":"select","options":["open","in-progress","done","cancelled"],"default":"open","terminal_options":["done","cancelled"],"abandoned_options":["cancelled"]},` +
			`{"key":"priority","label":"Priority","type":"select","options":["low","medium","high"]},` +
			`{"key":"estimate","label":"Estimate","type":"number"},` +
			`{"key":"due","label":"Due","type":"date"},` +
			`{"key":"follows","label":"Follows","type":"relation","collection":"tasks"}]}`,
	})
	post("/collections", map[string]any{
		"name": "Docs", "slug": "docs", "prefix": "DOC",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","published"],"default":"draft"}]}`,
	})
	// A collection archived with an item still in it (BUG-2884).
	post("/collections", map[string]any{
		"name": "Old Stuff", "slug": "old-stuff", "prefix": "OLD",
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	post("/collections/old-stuff/items", map[string]any{"title": "Archived survivor", "content": "Lives in an archived collection."})
	if rr := doRequest(srv, "DELETE", base+"/collections/old-stuff", nil); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("archive collection: %d %s", rr.Code, rr.Body.String())
	}

	// Items: a parent and children, fields, tags, a pin, a relation value.
	plan := post("/collections/tasks/items", map[string]any{"title": "Parent task", "content": "The parent.\n\nSee [[Child task one]].",
		"fields": `{"status":"in-progress","priority":"high","estimate":5,"due":"2026-11-01"}`, "tags": `["golden","parent"]`, "pinned": true})
	one := post("/collections/tasks/items", map[string]any{"title": "Child task one", "content": "First child.", "parent_id": plan["id"],
		"fields": `{"status":"done","priority":"low"}`})
	two := post("/collections/tasks/items", map[string]any{"title": "Child task two", "content": "Second child.", "parent_id": plan["id"],
		"fields": fmt.Sprintf(`{"status":"cancelled","follows":%q}`, one["id"])})
	doc := post("/collections/docs/items", map[string]any{"title": "Design doc", "content": "Draft one."})

	// Versions: two edits to the doc.
	for _, body := range []string{"Draft two.", "Draft three, with a table:\n\n| a | b |\n| - | - |\n| 1 | 2 |"} {
		if rr := doRequest(srv, "PATCH", base+"/items/"+doc["slug"].(string), map[string]any{"content": body}); rr.Code != http.StatusOK {
			t.Fatalf("edit doc: %d %s", rr.Code, rr.Body.String())
		}
		time.Sleep(1100 * time.Millisecond) // versions are throttled per second
	}

	// Comments.
	post("/items/"+plan["slug"].(string)+"/comments", map[string]any{"body": "First comment."})
	post("/items/"+plan["slug"].(string)+"/comments", map[string]any{"body": "Second comment, with **markdown**."})

	// Links: a block between live items, and one to an item later deleted.
	if _, err := st.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: two["id"].(string), LinkType: "blocks"}, one["id"].(string)); err != nil {
		t.Fatal(err)
	}
	gone := post("/collections/tasks/items", map[string]any{"title": "Deleted task", "content": "Soft-deleted after linking."})
	if _, err := st.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: gone["id"].(string), LinkType: "blocks"}, plan["id"].(string)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(gone["id"].(string)); err != nil {
		t.Fatal(err)
	}

	// Reminders: one armed in the future, one fired and acknowledged.
	if _, err := st.CreateReminder(ws.ID, plan["id"].(string), "2030-01-01T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	past, err := st.CreateReminder(ws.ID, one["id"].(string), time.Now().Add(2*time.Second).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, err := st.FireDueReminders(time.Now().UTC().Format(time.RFC3339), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AckReminder(ws.ID, past.ID); err != nil {
		t.Fatal(err)
	}

	// A collaborative edit set aside by a schema rebuild (BUG-3244): carried as
	// collab_set_aside, and it makes the item read content_state
	// superseded_set_aside. Written directly: producing one through the collab
	// path needs a schema-version change, which is frozen.
	if _, err := st.DB().Exec(`INSERT INTO item_yjs_updates_set_aside
		(item_id, op_log_id, update_data, schema_version, created_at, set_aside_at)
		VALUES (?, 1, ?, '1', '2026-10-01T10:00:00Z', '2026-10-02T10:00:00Z')`,
		two["id"], []byte{0x01, 0x02, 0x03, 0x04}); err != nil {
		t.Fatal(err)
	}

	// Conventions and playbooks (the blank template seeds the onboard one,
	// which carries a builtin_origin).
	post("/collections/conventions/items", map[string]any{"title": "Golden convention", "content": "Always run the round trip.",
		"fields": `{"status":"active","trigger":"always","priority":"must"}`})

	// Attachments: an image attached to an item and referenced from its body.
	rr = doMultipartUpload(srv, ws.Slug, "logo.png", realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var att struct {
		ID string `json:"id"`
	}
	parseJSON(t, rr, &att)
	if err := st.AttachAttachmentToItem(ws.ID, att.ID, doc["id"].(string)); err != nil {
		t.Fatal(err)
	}
	if rr := doRequest(srv, "PATCH", base+"/items/"+doc["slug"].(string), map[string]any{
		"content": "Draft three, with a table and a picture:\n\n![logo](pad-attachment:" + att.ID + ")"}); rr.Code != http.StatusOK {
		t.Fatalf("reference attachment: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", base+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export golden: %d %s", rr.Code, rr.Body.String())
	}
	files := readBundle(t, rr.Body.Bytes())
	if err := os.RemoveAll(interchangeGoldenDir); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if strings.HasSuffix(name, ".json") {
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			b, _ = json.MarshalIndent(v, "", "  ")
			b = append(b, '\n')
		}
		p := filepath.Join(interchangeGoldenDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %d golden bundle entries to %s", len(files), interchangeGoldenDir)
}
