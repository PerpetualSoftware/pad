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
	count  int // exactly how many differences the entry covers
	reason string
}{
	{`^attachments/attachment:[0-9a-f]{64}/created_at$`, 1,
		"import writes a new attachment row stamped now; the manifest's created_at is not restored"},
	{`^item_links/link:item:Parent task><unknown [0-9a-f-]{36}>\|blocks/(created_at|created_by|link_type|source_id|target_id)$`, 5,
		"a link whose endpoint is a soft-deleted item is exported (the raw graph) but skipped on import, because the item is not in the bundle (lead ruling on TASK-3628, day 92)"},
}

func TestInterchangeGolden_RoundTrip(t *testing.T) {
	if os.Getenv("PAD_UPDATE_INTERCHANGE_GOLDEN") != "" {
		writeInterchangeGolden(t)
	}
	golden := readInterchangeGolden(t)
	assertInterchangeGoldenCoverage(t, golden)

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

	used := make([]int, len(interchangeKnownLosses))
	var unexpected []string
	for _, d := range diffs {
		path := d[:strings.Index(d, "  (")]
		matched := false
		for i, l := range interchangeKnownLosses {
			if regexp.MustCompile(l.path).MatchString(path) {
				used[i]++
				matched = true
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
		if used[i] != l.count {
			t.Errorf("interchangeKnownLosses entry %q (%s) covers %d differences, want %d: stale, or a new loss at the same path", l.path, l.reason, used[i], l.count)
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

var interchangeUUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

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
	// Versions by their position in the item's history: the export orders them
	// by (created_at, version_seq), and two can share a second and a body.
	nth := map[string]int{}
	for _, v := range list(exp, "item_versions") {
		item := ref(str(v, "item_id"))
		nth[item]++
		key(str(v, "id"), fmt.Sprintf("version:%s#%d", item, nth[item]))
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
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Fatal("regenerate the interchange golden on SQLite: the generator writes a set-aside row with raw SQL")
	}
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
		"settings": `{"default_view":"board","board_group_by":"status"}`,
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
	// Written as an agent through the store: the HTTP door stamps the caller.
	tasksColl, err := st.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasksColl == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	parentID := plan["id"].(string)
	oneItem, err := st.CreateItem(ws.ID, tasksColl.ID, models.ItemCreate{Title: "Child task one", Content: "First child.", ParentID: &parentID,
		Fields: `{"status":"done","priority":"low"}`, CreatedBy: "agent", Source: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	one := map[string]any{"id": oneItem.ID, "slug": oneItem.Slug}
	two := post("/collections/tasks/items", map[string]any{"title": "Child task two", "content": "Second child.", "parent_id": plan["id"],
		"fields": fmt.Sprintf(`{"status":"cancelled","follows":%q}`, one["id"])})
	doc := post("/collections/docs/items", map[string]any{"title": "Design doc", "content": "Draft one."})

	// Versions: two edits to the doc, one by an agent. ForceVersion, because
	// a second edit by the same actor inside VersionThrottleInterval writes
	// no version row.
	editDoc := func(body, actor string) {
		t.Helper()
		if _, err := st.UpdateItem(doc["id"].(string), models.ItemUpdate{Content: &body, LastModifiedBy: actor, ForceVersion: true}); err != nil {
			t.Fatalf("edit doc: %v", err)
		}
	}
	editDoc("Draft two.", "user")
	editDoc("Draft three, with a table:\n\n| a | b |\n| - | - |\n| 1 | 2 |", "agent")

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
	editDoc("Draft three, with a table and a picture:\n\n![logo](pad-attachment:"+att.ID+")", "user")

	rr = doRequest(srv, "GET", base+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export golden: %d %s", rr.Code, rr.Body.String())
	}
	files := readBundle(t, rr.Body.Bytes())
	assertInterchangeGoldenCoverage(t, files)
	if t.Failed() {
		t.FailNow() // leave the committed golden alone
	}
	// Written beside the golden and swapped in, so a failure part way leaves
	// the committed golden in place.
	tmp := interchangeGoldenDir + ".new"
	if err := os.RemoveAll(tmp); err != nil {
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
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(interchangeGoldenDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, interchangeGoldenDir); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d golden bundle entries to %s", len(files), interchangeGoldenDir)
}

// assertInterchangeGoldenCoverage fails when the golden no longer carries one
// of the shapes the round trip exists to check, so a regeneration that lost
// one (a template change, a generator edit) cannot pass by checking less.
func assertInterchangeGoldenCoverage(t *testing.T, files map[string][]byte) {
	t.Helper()
	var exp models.WorkspaceExport
	if err := json.Unmarshal(files["pad-export.json"], &exp); err != nil {
		t.Fatalf("decode pad-export.json: %v", err)
	}
	var manifest models.AttachmentManifest
	if err := json.Unmarshal(files["attachments/manifest.json"], &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	has := map[string]bool{}
	collSlug := map[string]string{}
	for _, c := range exp.Collections {
		collSlug[c.ID] = c.Slug
		has["archived collection"] = has["archived collection"] || c.DeletedAt != ""
		has["collection settings"] = has["collection settings"] || strings.Contains(c.Settings, "default_view")
		has["relation field"] = has["relation field"] || strings.Contains(c.Schema, `"relation"`)
	}
	itemIDs := map[string]bool{}
	for _, it := range exp.Items {
		itemIDs[it.ID] = true
	}
	for _, it := range exp.Items {
		has["parent"] = has["parent"] || it.ParentID != ""
		has["pinned"] = has["pinned"] || it.Pinned
		has["tags"] = has["tags"] || (it.Tags != "" && it.Tags != "[]")
		has["agent author"] = has["agent author"] || it.CreatedBy == "agent"
		has["set-aside edit"] = has["set-aside edit"] || len(it.CollabSetAside) > 0
		has["content_state"] = has["content_state"] || it.ContentState != ""
		has["builtin origin"] = has["builtin origin"] || it.BuiltinOrigin != nil
		has["convention"] = has["convention"] || collSlug[it.CollectionID] == "conventions"
		has["playbook"] = has["playbook"] || collSlug[it.CollectionID] == "playbooks"
		for _, m := range interchangeUUID.FindAllString(it.Fields, -1) {
			has["relation value"] = has["relation value"] || itemIDs[m]
		}
		for _, a := range manifest.Entries {
			has["attachment ref in content"] = has["attachment ref in content"] || strings.Contains(it.Content, "pad-attachment:"+a.ID)
		}
	}
	for _, v := range exp.ItemVersions {
		has["edit version"] = has["edit version"] || !v.IsCreate
		has["agent version"] = has["agent version"] || v.CreatedBy == "agent"
	}
	for _, r := range exp.Reminders {
		has["armed reminder"] = has["armed reminder"] || r.FiredAt == ""
		has["acked reminder"] = has["acked reminder"] || r.AckedAt != ""
	}
	for _, l := range exp.ItemLinks {
		has["link to a dropped item"] = has["link to a dropped item"] || !itemIDs[l.TargetID]
		has["live link"] = has["live link"] || (itemIDs[l.SourceID] && itemIDs[l.TargetID])
	}
	has["comment"] = len(exp.Comments) > 0
	for _, a := range manifest.Entries {
		has["attached attachment"] = has["attached attachment"] || itemIDs[a.ItemID]
	}
	for _, want := range []string{
		"archived collection", "collection settings", "relation field", "parent", "pinned", "tags", "agent author",
		"set-aside edit", "content_state", "builtin origin", "convention", "playbook", "relation value",
		"attachment ref in content", "edit version", "agent version", "armed reminder", "acked reminder",
		"link to a dropped item", "live link", "comment", "attached attachment",
	} {
		if !has[want] {
			t.Errorf("the interchange golden no longer carries: %s", want)
		}
	}
}
