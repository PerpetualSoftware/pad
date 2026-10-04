package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// U2a (TASK-3390): app item writes. Every mutation runs under the write
// census, and every refusal must capture NOTHING.

const ticketSchema = `{"fields":[
	{"key":"status","label":"Status","type":"select","options":["open","pending","solved"],"terminal_options":["solved"],"default":"open"},
	{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
	{"key":"public_status","label":"Public status","type":"text"},
	{"key":"related","label":"Related","type":"relation","collection":"tickets"},
	{"key":"score","label":"Score","type":"number","computed":true},
	{"key":"external_id","label":"External id","type":"text","unique_scope":"workspace_collection"}
]}`

type appFixture struct {
	s         *store.Store
	a         *Store
	spec      store.FenceSpec
	ws        *models.Workspace
	owner     *models.User
	companion *models.Collection
	private   *models.Collection
	actor     store.FencedActor
}

func newAppFixture(t *testing.T, opts Options) appFixture {
	t.Helper()
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{Email: "owner-" + uuid.NewString()[:8] + "@example.com", Name: "Owner", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Apps", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	companion, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tickets", Schema: ticketSchema})
	if err != nil {
		t.Fatal(err)
	}
	private, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Private", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ETagKey == nil {
		opts.ETagKey = []byte("server-only-test-key-0123456789abcdef")
	}
	install := insertInstall(t, s, ws.ID)
	// A companion is a collection stamped with the install (as provisioning
	// leaves it); the fence checks the stamp in its transaction.
	if _, err := s.DB().Exec(s.D().Rebind(`UPDATE collections SET via_app = ? WHERE id = ?`), install, companion.ID); err != nil {
		t.Fatal(err)
	}
	return appFixture{
		s: s, a: New(s, opts), ws: ws, owner: owner, companion: companion, private: private,
		spec:  store.FenceSpec{InstallID: install, WorkspaceID: ws.ID, Epoch: 1, Companions: []string{companion.ID}},
		actor: store.FencedActor{Kind: "user", UserID: owner.ID},
	}
}

func insertInstall(t *testing.T, s *store.Store, workspaceID string) string {
	t.Helper()
	id := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB().Exec(s.D().Rebind(`INSERT INTO app_installs (id, workspace_id, origin, state, auth_epoch, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, ?, ?)`),
		id, workspaceID, "https://app.example", ts, ts); err != nil {
		t.Fatal(err)
	}
	return id
}

// allowedAppWrite is DOC-3371 §4's allowed-writes table for item writes. On
// SQLite the FTS5 shadow tables of items_fts are observed by name.
func allowedAppWrite(table string) bool {
	switch table {
	case "items", "item_versions", "activities", "event_outbox", "decision_jobs", "status_transitions", "item_wiki_links":
		return true
	}
	return strings.HasPrefix(table, "items_fts_")
}

func assertOnlyAllowed(t *testing.T, writes []storetest.Write) {
	t.Helper()
	for _, w := range writes {
		if !allowedAppWrite(w.Table) {
			t.Errorf("app write touched %s (%s), which the allowed-writes table does not permit", w.Table, w.Op)
		}
	}
}

func (f appFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.DB().QueryRow(f.s.D().Rebind(q), args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestAppCreateItem_WritesOnlyWhatTheSpecAllows(t *testing.T) {
	f := newAppFixture(t, Options{})
	var item *models.Item
	writes := storetest.CaptureWrites(t, f.s, func() {
		var err error
		item, err = f.a.CreateItem(context.Background(), f.spec, f.companion.ID, AppItemCreate{
			Title: "Printer is on fire", Content: "The printer in room 4.", Fields: map[string]any{"priority": "high"},
		}, f.actor)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	assertOnlyAllowed(t, writes)
	tables := storetest.Tables(writes)
	for _, want := range []string{"items", "item_versions", "activities", "event_outbox", "status_transitions"} {
		if !contains(tables, want) {
			t.Errorf("create did not write %s: %v", want, tables)
		}
	}

	// The row is the app's: source, attribution, the install, the default.
	var source, createdBy, createdVia, viaApp, userID string
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT source, created_by, created_via_app, via_app, created_by_user_id FROM items WHERE id = ?`), item.ID).
		Scan(&source, &createdBy, &createdVia, &viaApp, &userID); err != nil {
		t.Fatal(err)
	}
	if source != "app" || createdBy != "user" || createdVia != f.spec.InstallID || viaApp != f.spec.InstallID || userID != f.owner.ID {
		t.Errorf("item attribution: source=%s created_by=%s created_via_app=%s via_app=%s user=%s", source, createdBy, createdVia, viaApp, userID)
	}
	if fieldValue(t, item.Fields, "status") != "open" {
		t.Errorf("schema default not applied: %s", item.Fields)
	}
	if !strings.HasPrefix(item.Slug, "printer-is-on-fire-") || len(item.Slug) != len("printer-is-on-fire-")+6 {
		t.Errorf("slug %q is not the title plus a random suffix", item.Slug)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_versions WHERE item_id = ? AND via_app = ? AND source = 'app'`, item.ID, f.spec.InstallID); n != 1 {
		t.Errorf("initial version rows with the install: %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM activities WHERE document_id = ? AND action = 'created' AND via_app = ?`, item.ID, f.spec.InstallID); n != 1 {
		t.Errorf("created activities with the install: %d", n)
	}

	// The event carries the install in the frozen projection block.
	var payload string
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT payload FROM event_outbox WHERE subject_id = ? AND event_type = 'item.created'`), item.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Proj struct {
			Creator struct {
				ViaApp string `json:"via_app"`
			} `json:"creator"`
		} `json:"app_projection"`
	}
	if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev.Proj.Creator.ViaApp != f.spec.InstallID {
		t.Errorf("projection creator.via_app = %q (%v)", ev.Proj.Creator.ViaApp, err)
	}
}

func TestAppUpdateItem_WritesOnlyWhatTheSpecAllows(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	item, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Ticket"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	var updated *models.Item
	writes := storetest.CaptureWrites(t, f.s, func() {
		updated, err = f.a.UpdateItem(ctx, f.spec, item.ID, AppItemUpdate{
			FieldsPatch: map[string]any{"status": "solved", "public_status": "Fixed"}, ExpectedETag: mustETag(t, f.a, f.spec, item),
		}, f.actor)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	assertOnlyAllowed(t, writes)
	if contains(storetest.Tables(writes), "item_versions") {
		t.Error("a fields-only update wrote a version row")
	}
	if fieldValue(t, updated.Fields, "status") != "solved" || updated.Seq <= item.Seq {
		t.Errorf("update not applied: %s seq %d -> %d", updated.Fields, item.Seq, updated.Seq)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM status_transitions WHERE item_id = ? AND from_status = 'open' AND to_status = 'solved'`, item.ID); n != 1 {
		t.Errorf("status transitions: %d", n)
	}
	// A second update inside the debounce window merges into one activity.
	if _, err := f.a.UpdateItem(ctx, f.spec, item.ID, AppItemUpdate{
		FieldsPatch: map[string]any{"priority": "low"}, ExpectedETag: mustETag(t, f.a, f.spec, updated),
	}, f.actor); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM activities WHERE document_id = ? AND action = 'updated' AND via_app = ?`, item.ID, f.spec.InstallID); n != 1 {
		t.Errorf("updated activities after two quick updates: %d, want 1 (debounced)", n)
	}
}

// Every refusal is decided before anything is written: the capture is empty.
func TestAppItemRefusalsWriteNothing(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	mine, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Mine"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	human, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human ticket", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	stale := f.spec
	stale.Epoch = 2
	etag := mustETag(t, f.a, f.spec, mine)

	for name, tc := range map[string]struct {
		run  func() error
		want func(error) bool
	}{
		"stale epoch": {func() error {
			_, err := f.a.CreateItem(ctx, stale, f.companion.ID, AppItemCreate{Title: "x"}, f.actor)
			return err
		}, func(err error) bool { return errors.Is(err, store.ErrFenceStale) }},
		"non-companion collection": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.private.ID, AppItemCreate{Title: "x"}, f.actor)
			return err
		}, func(err error) bool { return errors.Is(err, store.ErrNotCompanion) }},
		"undeclared field": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Fields: map[string]any{"nope": "1"}}, f.actor)
			return err
		}, IsInputError},
		"relation field": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Fields: map[string]any{"related": mine.ID}}, f.actor)
			return err
		}, IsInputError},
		"computed field": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Fields: map[string]any{"score": 3}}, f.actor)
			return err
		}, IsInputError},
		"unique-scoped field": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Fields: map[string]any{"external_id": "e1"}}, f.actor)
			return err
		}, IsInputError},
		"attachment token in content": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Content: "see ![](PAD-ATTACHMENT:abc)"}, f.actor)
			return err
		}, IsInputError},
		"attachment token in a field": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Fields: map[string]any{"public_status": "pad-attachment:abc"}}, f.actor)
			return err
		}, IsInputError},
		"workspace-qualified link": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x", Content: "[[" + f.ws.Slug + "::TICKE-1]]"}, f.actor)
			return err
		}, IsInputError},
		"bad actor": {func() error {
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "x"}, store.FencedActor{Kind: "system", UserID: f.owner.ID})
			return err
		}, func(err error) bool { return errors.Is(err, store.ErrAppBadActor) }},
		"update: item a human created": {func() error {
			_, err := f.a.UpdateItem(ctx, f.spec, human.ID, AppItemUpdate{FieldsPatch: map[string]any{"priority": "low"}, ExpectedETag: mustETag(t, f.a, f.spec, human)}, f.actor)
			return err
		}, func(err error) bool { return errors.Is(err, store.ErrAppNotAppItem) }},
		"update: stale etag": {func() error {
			_, err := f.a.UpdateItem(ctx, f.spec, mine.ID, AppItemUpdate{FieldsPatch: map[string]any{"priority": "low"}, ExpectedETag: "0000"}, f.actor)
			return err
		}, func(err error) bool { return errors.Is(err, store.ErrAppETagMismatch) }},
		"update: relation key": {func() error {
			_, err := f.a.UpdateItem(ctx, f.spec, mine.ID, AppItemUpdate{FieldsPatch: map[string]any{"related": mine.ID}, ExpectedETag: etag}, f.actor)
			return err
		}, IsInputError},
		"update: bad option": {func() error {
			_, err := f.a.UpdateItem(ctx, f.spec, mine.ID, AppItemUpdate{FieldsPatch: map[string]any{"priority": "urgent"}, ExpectedETag: etag}, f.actor)
			return err
		}, IsInputError},
		"update: empty patch": {func() error {
			_, err := f.a.UpdateItem(ctx, f.spec, mine.ID, AppItemUpdate{ExpectedETag: etag}, f.actor)
			return err
		}, IsInputError},
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			writes := storetest.CaptureWrites(t, f.s, func() { got = tc.run() })
			if got == nil || !tc.want(got) {
				t.Fatalf("got %v", got)
			}
			if len(writes) != 0 {
				t.Fatalf("a refusal wrote: %v", writes)
			}
		})
	}
}

// On Pad Cloud the item cap applies inside the fence, and its refusal names
// no count.
func TestAppCreateItem_ItemCapIsEnforcedWithoutACount(t *testing.T) {
	f := newAppFixture(t, Options{PlanLimit: true})
	if _, err := f.s.SetUserPlan(f.owner.ID, store.PlanWrite{Plan: "free", Source: store.PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetUserPlanOverrides(f.owner.ID, `{"items_per_workspace":1}`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "First"}, f.actor); err != nil {
		t.Fatalf("first create: %v", err)
	}
	var got error
	writes := storetest.CaptureWrites(t, f.s, func() {
		_, got = f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Second"}, f.actor)
	})
	if !errors.Is(got, store.ErrAppItemLimit) || strings.ContainsAny(got.Error(), "0123456789") {
		t.Fatalf("over the cap: %v", got)
	}
	if len(writes) != 0 {
		t.Fatalf("a capped create wrote: %v", writes)
	}
	// Self-host (PlanLimit off) is not capped, as on the human path.
	free := New(f.s, Options{ETagKey: []byte("server-only-test-key-0123456789abcdef")})
	if _, err := free.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Third"}, f.actor); err != nil {
		t.Fatalf("self-host create over the cap: %v", err)
	}
}

// An app link names only companion items: a link to a hidden item stays
// broken, a link to a companion resolves.
func TestAppCreateItem_LinksResolveOnlyToCompanions(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	if _, err := f.s.CreateItem(f.ws.ID, f.private.ID, models.ItemCreate{Title: "Salary review"}); err != nil {
		t.Fatal(err)
	}
	visible, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Known issue"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Report", Content: "See [[Salary review]] and [[Known issue]]."}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ? AND target_item_id IS NULL`, item.ID); n != 1 {
		t.Errorf("broken links: %d, want 1 (the hidden item)", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ? AND target_item_id = ?`, item.ID, visible.ID); n != 1 {
		t.Errorf("resolved companion links: %d, want 1", n)
	}
}

// App and human creates interleaved: every item gets its own item_number and
// seq. The meaningful run is on Postgres (make test-pg), where the
// workspace advisory lock is what serializes them.
func TestAppAndHumanCreatesNeverShareNumbers(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "App"}, f.actor)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human", Fields: `{"status":"open"}`})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) - COUNT(DISTINCT item_number) FROM items WHERE workspace_id = ?`, f.ws.ID); n != 0 {
		t.Errorf("%d duplicate item numbers", n)
	}
	if n := f.count(t, `SELECT COUNT(*) - COUNT(DISTINCT seq) FROM items WHERE workspace_id = ?`, f.ws.ID); n != 0 {
		t.Errorf("%d duplicate seq values", n)
	}
}

func mustETag(t *testing.T, a *Store, spec store.FenceSpec, item *models.Item) string {
	t.Helper()
	e, err := a.ETag(spec, item)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// fieldValue decodes a fields blob: Postgres JSONB and SQLite TEXT render
// the same object differently.
func fieldValue(t *testing.T, fieldsJSON, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("fields %q: %v", fieldsJSON, err)
	}
	return m[key]
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// A hidden item must not change what an app link resolves to (codex round 1):
// with the unrestricted resolver, a private item sharing a companion's title
// could win the match and leave the link broken, which told the app it
// existed.
func TestAppLinks_HiddenItemsDoNotInfluenceResolution(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	known, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Known"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"Known", "Known|Secret"} {
		if _, err := f.s.CreateItem(f.ws.ID, f.private.ID, models.ItemCreate{Title: hidden}); err != nil {
			t.Fatal(err)
		}
	}
	for _, content := range []string{"[[Known]]", "[[Known|Secret]]"} {
		item, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Links " + content, Content: content}, f.actor)
		if err != nil {
			t.Fatal(err)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ? AND target_item_id = ?`, item.ID, known.ID); n != 1 {
			t.Errorf("%s: resolved to the companion %d times, want 1", content, n)
		}
	}
}

// Schema defaults are applied after the key checks, so they must not carry a
// forbidden value onto an app item (codex round 1).
func TestAppCreateItem_DefaultsCannotBypassTheRules(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	target, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Target"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	withDefaults, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Defaulted", Schema: `{"fields":[
		{"key":"related","label":"Related","type":"relation","collection":"tickets","default":"` + target.ID + `"},
		{"key":"score","label":"Score","type":"number","computed":true,"default":3},
		{"key":"external_id","label":"Ext","type":"text","unique_scope":"workspace_collection","default":"e1"},
		{"key":"note","label":"Note","type":"text","default":"plain"}
	]}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE collections SET via_app = ? WHERE id = ?`), f.spec.InstallID, withDefaults.ID); err != nil {
		t.Fatal(err)
	}
	spec := f.spec
	spec.Companions = append(append([]string{}, spec.Companions...), withDefaults.ID)
	item, err := f.a.CreateItem(ctx, spec, withDefaults.ID, AppItemCreate{Title: "Defaulted"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"related", "score", "external_id"} {
		if v := fieldValue(t, item.Fields, k); v != nil {
			t.Errorf("default for forbidden field %s landed: %v", k, v)
		}
	}
	if fieldValue(t, item.Fields, "note") != "plain" {
		t.Errorf("an ordinary default was not applied: %s", item.Fields)
	}

	tokenDefault, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Tokened", Schema: `{"fields":[
		{"key":"note","label":"Note","type":"text","default":"pad-attachment:abc"}
	]}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE collections SET via_app = ? WHERE id = ?`), f.spec.InstallID, tokenDefault.ID); err != nil {
		t.Fatal(err)
	}
	spec.Companions = append(spec.Companions, tokenDefault.ID)
	var got error
	writes := storetest.CaptureWrites(t, f.s, func() {
		_, got = f.a.CreateItem(ctx, spec, tokenDefault.ID, AppItemCreate{Title: "Tokened"}, f.actor)
	})
	if !IsInputError(got) || len(writes) != 0 {
		t.Fatalf("an attachment token in a default: %v, writes %v", got, writes)
	}
}

// Without a usable server key there are no etags to issue or check: a
// forgeable token is worse than none (codex round 1).
func TestAppUpdateItem_RefusesWithoutAnETagKey(t *testing.T) {
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	item, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Keyless"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{nil, []byte("short")} {
		keyless := New(f.s, Options{ETagKey: key})
		if _, err := keyless.ETag(f.spec, item); !errors.Is(err, ErrNoETagKey) {
			t.Errorf("ETag with key %q: %v", key, err)
		}
		if _, err := keyless.UpdateItem(ctx, f.spec, item.ID, AppItemUpdate{FieldsPatch: map[string]any{"priority": "low"}, ExpectedETag: "x"}, f.actor); !errors.Is(err, ErrNoETagKey) {
			t.Errorf("UpdateItem with key %q: %v", key, err)
		}
	}
}
