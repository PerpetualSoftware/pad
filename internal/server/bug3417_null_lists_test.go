package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/appstore"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3417: every list in a JSON response the apps feature added is a list,
// never null. The contract (and the TS types) say arrays, and a Go nil slice
// serializes as null: a real manifest with no item actions or artifacts
// crashed the install review that way.
//
// nullLists walks a decoded body alongside the Go type that produced it and
// names every slice-typed field (or top-level list) that arrived as null.
// The type is the census: a new list field is covered the moment it is added
// to one of these response types.
func nullLists(t reflect.Type, v any, path string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	rawMessage := reflect.TypeOf(json.RawMessage(nil))
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		if t == rawMessage || t.Elem().Kind() == reflect.Uint8 {
			return
		}
		if v == nil {
			*out = append(*out, path)
			return
		}
		arr, _ := v.([]any)
		for i, e := range arr {
			nullLists(t.Elem(), e, path+"["+strconv.Itoa(i)+"]", out)
		}
	case reflect.Map:
		m, _ := v.(map[string]any)
		for k, e := range m {
			nullLists(t.Elem(), e, path+"."+k, out)
		}
	case reflect.Struct:
		if t == reflect.TypeOf(time.Time{}) {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" {
				nullLists(f.Type, v, path, out)
				continue
			}
			if name == "" {
				name = f.Name
			}
			if e, present := m[name]; present {
				nullLists(f.Type, e, path+"."+name, out)
			}
		}
	}
}

func assertNoNullLists(t *testing.T, where string, rr *httptest.ResponseRecorder, want int, typ reflect.Type) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("%s: %d %s", where, rr.Code, rr.Body.String())
	}
	var v any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s: %v (%s)", where, err, rr.Body.String())
	}
	var found []string
	nullLists(typ, v, "$", &found)
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("%s: list fields serialized as null: %v\n%s", where, found, rr.Body.String())
	}
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// minimalManifest is the smallest valid apps/1 manifest: one companion
// collection, no events, no webhook, no item actions, no artifacts.
func (e *appsEnv) minimalManifest(version string) map[string]any {
	return map[string]any{
		"id": "acme/minimal", "version": version, "min_contract": map[string]any{"apps": 1, "events": 1},
		"title": "Minimal", "publisher": "Acme", "base_url": e.origin(),
		"redirect_uris": []any{e.origin() + "/oauth/callback"},
		"scopes":        map[string]any{"service": map[string]any{"access": "write"}, "delegated": map[string]any{"access": "read"}},
		"companion_pack": map[string]any{
			"collections": []any{map[string]any{"key": "notes", "slug": "minimal-notes", "name": "Minimal notes",
				"schema": map[string]any{"fields": []any{map[string]any{"key": "status", "label": "Status", "type": "select", "options": []any{"open", "done"}}}}}},
		},
	}
}

func TestBug3417_AppsResponsesHaveNoNullLists(t *testing.T) {
	t.Run("owner and admin doors over a minimal install", func(t *testing.T) {
		e := newProvisionEnv(t)
		e.publish(t, e.minimalManifest("1.0.0"))
		ws := "/api/v1/workspaces/" + e.ws

		rr := e.preview(t)
		assertNoNullLists(t, "install preview", rr, http.StatusOK, typeOf[appPreview]())
		var p appPreview
		parseJSON(t, rr, &p)
		assertNoNullLists(t, "pending preview", doRequestWithCookie(e.srv, "GET", ws+"/apps/install/pending/"+p.PendingID, nil, e.token),
			http.StatusOK, typeOf[appPreview]())

		rr = e.confirm(t, p, p.ManifestSHA256)
		assertNoNullLists(t, "install confirm", rr, http.StatusCreated, typeOf[appInstallConfirmResponse]())
		var c appInstallConfirmResponse
		parseJSON(t, rr, &c)

		assertNoNullLists(t, "apps list", doRequestWithCookie(e.srv, "GET", ws+"/apps", nil, e.token),
			http.StatusOK, typeOf[appInstallListResponse]())
		assertNoNullLists(t, "app view", doRequestWithCookie(e.srv, "GET", ws+"/apps/"+c.InstallID, nil, e.token),
			http.StatusOK, typeOf[appInstallStateResponse]())
		assertNoNullLists(t, "admin apps settings", doRequestWithCookie(e.srv, "GET", "/api/v1/admin/apps", nil, e.token),
			http.StatusOK, typeOf[appsSettingsResponse]())

		e.publish(t, e.minimalManifest("1.0.1"))
		assertNoNullLists(t, "upgrade preview", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/upgrade/preview", nil, e.token),
			http.StatusOK, typeOf[appPreview]())

		assertNoNullLists(t, "disable", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/disable", nil, e.token),
			http.StatusOK, typeOf[appInstallStateResponse]())
		assertNoNullLists(t, "enable", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/enable", nil, e.token),
			http.StatusOK, typeOf[appInstallStateResponse]())
	})

	t.Run("app API and app actions with empty lists", func(t *testing.T) {
		f := appAPIFixture(t, "write")
		// An item the app owns, with no comments and no attachments.
		rr := appDo(f.srv, "POST", f.path("/collections/requests/items"), f.token, map[string]any{"title": "Fresh"})
		assertNoNullLists(t, "app item create", rr, http.StatusCreated, typeOf[AppItem]())
		var it AppItem
		parseJSON(t, rr, &it)

		assertNoNullLists(t, "app collections", appGet(f.srv, f.path("/collections"), f.token), http.StatusOK,
			typeOf[struct {
				Collections []AppCollection `json:"collections"`
			}]())
		assertNoNullLists(t, "app collection", appGet(f.srv, f.path("/collections/requests"), f.token), http.StatusOK, typeOf[AppCollection]())
		// A readable collection with no items.
		assertNoNullLists(t, "app items (empty)", appGet(f.srv, f.path("/collections/"+f.system.Slug+"/items"), f.token), http.StatusOK,
			typeOf[struct {
				Items []AppItem `json:"items"`
			}]())
		assertNoNullLists(t, "app item", appGet(f.srv, f.path("/items/"+it.ID), f.token), http.StatusOK, typeOf[AppItem]())
		assertNoNullLists(t, "app comments (empty)", appGet(f.srv, f.path("/items/"+it.ID+"/comments"), f.token), http.StatusOK,
			typeOf[struct {
				Comments []AppComment `json:"comments"`
			}]())
		assertNoNullLists(t, "app me", appGet(f.srv, f.path("/me"), f.token), http.StatusOK, typeOf[AppMe]())
		assertNoNullLists(t, "app upload", appUpload(f, "/items/"+it.ID+"/attachments?filename=a.png", testPNG(t), int64(len(testPNG(t)))),
			http.StatusCreated, typeOf[appstore.AppAttachment]())

		// The human item-actions list on an item no app offers an action on.
		viewer, tok := loginTestUserAs(t, f.srv, "viewer-3417@example.com", "Vera", "pw-3417-viewer")
		if _, err := f.srv.store.DB().Exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'viewer', 'all', ?)`,
			f.ws.ID, viewer.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		assertNoNullLists(t, "item app actions (none)",
			doRequestWithCookie(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+f.item.ID+"/app-actions", nil, tok),
			http.StatusOK, typeOf[[]store.ItemAppAction]())
	})
}
