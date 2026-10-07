package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3446: the `blank` template seeds Conventions with trigger [always] and
// scope [all], and Playbooks with trigger [manual] and scope [all]. Every
// triggered library convention was refused there, through every door, so an
// agent following the onboard playbook literally failed at B3. These tests
// replay the playbook's B3 and B5 writes against a blank workspace, in the
// request shapes each door sends:
//
//   - CLI `pad library activate` / remote MCP `pad_library.activate`:
//     models.BuildConventionItemCreate + the typed `convention` member;
//   - POST /library/activate, which the web Library page's Activate button,
//     the CLI against a current server, and remote MCP all use since
//     TASK-3462 (the web's own client-built fields blob is gone);
//   - B3's literal `pad item create conventions --field trigger=<t>
//     --field status=active`.
//
// The MCP door is also driven through the real dispatcher in
// internal/mcp/bug3446_blank_library_activate_test.go.

func bug3446BlankWorkspace(t *testing.T, srv *Server, name string) (slug, wsID string) {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name, "template": "blank"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create blank workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	return ws.Slug, ws.ID
}

func bug3446Options(t *testing.T, srv *Server, wsID, collSlug, key string) []string {
	t.Helper()
	coll, err := srv.store.GetCollectionBySlug(wsID, collSlug)
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug %s: %v", collSlug, err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, f := range schema.Fields {
		if f.Key == key {
			out := append([]string(nil), f.Options...)
			sort.Strings(out)
			return out
		}
	}
	t.Fatalf("%s has no %q field", collSlug, key)
	return nil
}

// cliConventionBody is what `pad library activate` and pad_library.activate
// post for a library convention.
func cliConventionBody(t *testing.T, c collections.LibraryConvention) map[string]any {
	t.Helper()
	fields, conv, err := models.BuildConventionItemCreate("active", &models.ItemConventionMetadata{
		Category: c.Category, Trigger: c.Trigger, Surfaces: c.Surfaces, Enforcement: c.Enforcement, Commands: c.Commands,
	})
	if err != nil {
		t.Fatalf("BuildConventionItemCreate: %v", err)
	}
	return map[string]any{"title": c.Title, "content": c.Content, "fields": fields, "convention": conv}
}

func playbookBody(p collections.LibraryPlaybook) map[string]any {
	f := map[string]any{"status": "active", "trigger": p.Trigger, "scope": p.Scope}
	if p.InvocationSlug != "" {
		f["invocation_slug"] = p.InvocationSlug
	}
	if len(p.Arguments) > 0 {
		f["arguments"] = p.Arguments
	}
	fields, _ := json.Marshal(f)
	return map[string]any{"title": p.Title, "content": p.Content, "fields": string(fields)}
}

func libraryConventions() []collections.LibraryConvention {
	var out []collections.LibraryConvention
	for _, cat := range collections.ConventionLibrary() {
		out = append(out, cat.Conventions...)
	}
	return out
}

// usedWords is the set of values for key the given writes carry, plus what
// the blank template seeded: the options must end EXACTLY there.
func usedWords(seed []string, vals []string) []string {
	set := map[string]bool{}
	for _, v := range append(append([]string(nil), seed...), vals...) {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Every library convention and playbook activates in a blank workspace, in
// the CLI/MCP shape and in the web shape, and the options end at exactly the
// words those activations used.
func TestBUG3446_EveryLibraryEntryActivatesInABlankWorkspace(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		for _, door := range []struct {
			name string
			post func(t *testing.T, slug string, c collections.LibraryConvention) (int, string)
		}{
			{"cli-mcp", func(t *testing.T, slug string, c collections.LibraryConvention) (int, string) {
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", cliConventionBody(t, c))
				return rr.Code, rr.Body.String()
			}},
			{"activate", func(_ *testing.T, slug string, c collections.LibraryConvention) (int, string) {
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", map[string]string{"key": c.Key})
				return rr.Code, rr.Body.String()
			}},
		} {
			t.Run(door.name, func(t *testing.T) {
				slug, wsID := bug3446BlankWorkspace(t, srv, "Blank 3446 "+door.name)
				var triggers, scopes []string
				for _, c := range libraryConventions() {
					if code, body := door.post(t, slug, c); code != http.StatusCreated {
						t.Errorf("activate convention %q (trigger %s): %d %s", c.Title, c.Trigger, code, body)
						continue
					}
					triggers = append(triggers, c.Trigger)
					if len(c.Surfaces) > 0 {
						scopes = append(scopes, c.Surfaces[0])
					}
				}
				if got, want := bug3446Options(t, srv, wsID, "conventions", "trigger"), usedWords(collections.BlankConventionTriggers, triggers); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("conventions trigger options = %v, want exactly the seed plus the words used %v", got, want)
				}
				if got, want := bug3446Options(t, srv, wsID, "conventions", "scope"), usedWords(collections.BlankConventionScopes, scopes); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("conventions scope options = %v, want exactly the seed plus the words used %v", got, want)
				}

				// B5. A library playbook whose title the workspace already
				// holds (the seeded onboard playbook) is what the UI shows as
				// "Active"; it is not activated twice.
				existing := map[string]bool{}
				rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/playbooks/items", nil)
				var seeded []models.Item
				parseJSON(t, rr, &seeded)
				for _, it := range seeded {
					existing[it.Title] = true
				}
				for _, cat := range collections.PlaybookLibrary() {
					for _, p := range cat.Playbooks {
						if existing[p.Title] {
							continue
						}
						rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/playbooks/items", playbookBody(p))
						if rr.Code != http.StatusCreated {
							t.Errorf("activate playbook %q (trigger %s): %d %s", p.Title, p.Trigger, rr.Code, rr.Body.String())
						}
					}
				}
			})
		}
	})
}

// B3's literal step, the report, and the bound: the first write that needs a
// word adds it and says so in its warnings and its activity; the next write
// with the same word adds nothing; a word the library does not use is refused
// as before and adds nothing.
func TestBUG3446_LiteralItemCreateWidensOnlyByLibraryWordsAndRecordsIt(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := bug3446BlankWorkspace(t, srv, "Blank 3446 literal")
		path := "/api/v1/workspaces/" + slug + "/collections/conventions/items"
		create := func(title, trigger string) (*models.Item, int, string) {
			fields, _ := json.Marshal(map[string]string{"trigger": trigger, "status": "active"})
			rr := doRequest(srv, "POST", path, map[string]any{"title": title, "content": "x", "fields": string(fields)})
			if rr.Code != http.StatusCreated {
				return nil, rr.Code, rr.Body.String()
			}
			var it models.Item
			parseJSON(t, rr, &it)
			return &it, rr.Code, ""
		}

		first, code, body := create("Run make test before done", "on-task-complete")
		if first == nil {
			t.Fatalf("B3 literal create with a library trigger: %d %s", code, body)
		}
		if first.Warnings == nil || strings.Join(first.Warnings.OptionsAdded["trigger"], ",") != "on-task-complete" {
			t.Errorf("first create: warnings.options_added = %+v, want trigger: [on-task-complete]", first.Warnings)
		}
		acts, err := srv.store.ListDocumentActivity(first.ID, models.ActivityListParams{})
		if err != nil {
			t.Fatalf("ListDocumentActivity: %v", err)
		}
		recorded := false
		for _, a := range acts {
			// Decoded, not substring-matched: Postgres stores metadata as
			// JSONB and re-serialises it with its own spacing.
			var meta map[string]string
			if a.Action != "created" || json.Unmarshal([]byte(a.Metadata), &meta) != nil {
				continue
			}
			if meta["options_added"] == "trigger: on-task-complete" && meta["options_added_collection"] == "Conventions" {
				recorded = true
			}
		}
		if !recorded {
			t.Errorf("the created activity does not record the options added: %+v", acts)
		}

		second, code, body := create("Summarize on done", "on-task-complete")
		if second == nil {
			t.Fatalf("second create: %d %s", code, body)
		}
		if second.Warnings != nil && len(second.Warnings.OptionsAdded) > 0 {
			t.Errorf("second create with an already-listed word reported options_added %+v", second.Warnings.OptionsAdded)
		}

		if it, code, body := create("Invented rule", "on-experiment-run"); it != nil || code != http.StatusBadRequest || !strings.Contains(body, "on-experiment-run") {
			t.Errorf("a trigger outside the library vocabulary: got %d %s, want 400 naming it", code, body)
		}
		if got := bug3446Options(t, srv, wsID, "conventions", "trigger"); strings.Join(got, ",") != "always,on-task-complete" {
			t.Errorf("trigger options = %v, want [always on-task-complete]", got)
		}
	})
}

// Ruling (a): an EDITOR's create may widen, bounded the same way.
func TestBUG3446_EditorCreateWidens(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := bug3446BlankWorkspace(t, srv, "Blank 3446 editor")
		owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner-3446@example.com", Name: "Owner", Username: "owner-3446", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser owner: %v", err)
		}
		editor, err := srv.store.CreateUser(models.UserCreate{Email: "editor-3446@example.com", Name: "Editor", Username: "editor-3446", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser editor: %v", err)
		}
		if err := srv.store.AddWorkspaceMember(wsID, owner.ID, "owner"); err != nil {
			t.Fatalf("AddWorkspaceMember owner: %v", err)
		}
		if err := srv.store.AddWorkspaceMember(wsID, editor.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember editor: %v", err)
		}
		token, err := srv.store.CreateSession(editor.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		c := collections.GetLibraryConvention("Conventional commit format")
		if c == nil {
			t.Fatal("library convention missing")
		}
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", cliConventionBody(t, *c), token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("editor activate: %d %s", rr.Code, rr.Body.String())
		}
		if got := bug3446Options(t, srv, wsID, "conventions", "trigger"); strings.Join(got, ",") != "always,"+c.Trigger {
			t.Errorf("trigger options after the editor's activate = %v", got)
		}
	})
}

// The artifact import door keeps its documented clear-and-warn for a select
// value the destination does not list; it never widens (codex round 1).
func TestBUG3446_ArtifactImportDoesNotWiden(t *testing.T) {
	srv := testServer(t)
	slug, wsID := bug3446BlankWorkspace(t, srv, "Blank 3446 import")
	data, err := artifact.Encode(artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Imported commit rule",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all"},
		Body:          "body\n",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `cleared on import`) {
		t.Errorf("import did not report clearing the unlisted trigger: %s", rr.Body.String())
	}
	if got := bug3446Options(t, srv, wsID, "conventions", "trigger"); strings.Join(got, ",") != "always" {
		t.Errorf("an import widened the trigger options: %v", got)
	}
}

// The schema rewrite touches the FIRST definition of a key, the one
// validation reads, and never duplicates a value (codex round 1).
func TestBUG3446_AppendSelectOptionsJSON(t *testing.T) {
	raw := `{"fields":[{"key":"trigger","type":"select","options":["always"],"x_extra":1},{"key":"trigger","type":"select","options":["always","on-commit"]}]}`
	out, err := appendSelectOptionsJSON(raw, map[string][]string{"trigger": {"on-commit", "always"}})
	if err != nil {
		t.Fatalf("appendSelectOptionsJSON: %v", err)
	}
	want := `{"fields":[{"key":"trigger","options":["always","on-commit"],"type":"select","x_extra":1},{"key":"trigger","options":["always","on-commit"],"type":"select"}]}`
	if out != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// A create that the widened schema would still refuse writes nothing to the
// schema: it is validated against the widened schema before the widening is
// stored (codex round 2). Here the trigger is a library word but the status
// is not a valid option.
func TestBUG3446_RefusedCreateDoesNotWiden(t *testing.T) {
	srv := testServer(t)
	slug, wsID := bug3446BlankWorkspace(t, srv, "Blank 3446 refused")
	fields, _ := json.Marshal(map[string]string{"trigger": "on-commit", "status": "not-a-status"})
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/conventions/items", map[string]any{"title": "Refused", "fields": string(fields)})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "not-a-status") {
		t.Fatalf("create with a bad status: got %d %s, want 400 naming it", rr.Code, rr.Body.String())
	}
	if got := bug3446Options(t, srv, wsID, "conventions", "trigger"); strings.Join(got, ",") != "always" {
		t.Errorf("a refused create widened the trigger options: %v", got)
	}
}
