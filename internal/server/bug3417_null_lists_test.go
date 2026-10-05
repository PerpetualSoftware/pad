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
// What it cannot see, by design: the contents of interface-typed values
// (an item's `fields` map is the item's own data, whose values the schema
// types), and json.RawMessage. Element types are walked only through the
// elements present, so each door is driven twice where it can be: by the
// minimal manifest (the empty lists the bug was about) and by the full one
// (every list filled, so the lists inside its elements are walked too).
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
		rr = doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/upgrade/preview", nil, e.token)
		assertNoNullLists(t, "upgrade preview", rr, http.StatusOK, typeOf[appPreview]())
		var up appPreview
		parseJSON(t, rr, &up)
		// An upgrade that adds no artifacts (codex r1).
		assertNoNullLists(t, "upgrade confirm", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/upgrade/confirm",
			map[string]any{"pending_id": up.PendingID, "manifest_sha256": up.ManifestSHA256}, e.token),
			http.StatusOK, typeOf[appUpgradeConfirmResponse]())

		// A webhook-only private origin may omit `allowed` (codex r1): the
		// PUT and every later GET answer it as a list.
		assertNoNullLists(t, "admin apps settings PUT", doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{
			"private_origins": []map[string]any{
				{"origin": e.app.URL, "allowed": []string{"127.0.0.1"}, "fetch": true},
				{"origin": "https://hooks.internal.example", "webhook": true},
			},
		}, e.token), http.StatusOK, typeOf[appsSettingsResponse]())
		assertNoNullLists(t, "admin apps settings GET", doRequestWithCookie(e.srv, "GET", "/api/v1/admin/apps", nil, e.token),
			http.StatusOK, typeOf[appsSettingsResponse]())

		assertNoNullLists(t, "disable", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/disable", nil, e.token),
			http.StatusOK, typeOf[appInstallStateResponse]())
		assertNoNullLists(t, "enable", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/enable", nil, e.token),
			http.StatusOK, typeOf[appInstallStateResponse]())
	})

	// The full manifest (events, an item action, an artifact), so the nested
	// lists inside those elements are walked too, and an upgrade that changes
	// the artifact, so its review's lists are (codex r2).
	t.Run("full manifest: preview, confirm, changed-artifact upgrade, lifecycle", func(t *testing.T) {
		e := newProvisionEnv(t)
		ws := "/api/v1/workspaces/" + e.ws
		rr := e.preview(t)
		assertNoNullLists(t, "full preview", rr, http.StatusOK, typeOf[appPreview]())
		var p appPreview
		parseJSON(t, rr, &p)
		if len(p.Events) == 0 || len(p.ItemActions) == 0 || len(p.Artifacts) == 0 {
			t.Fatalf("the full manifest's preview has empty lists, so nothing nested was walked: %+v", p)
		}
		rr = e.confirm(t, p, p.ManifestSHA256)
		assertNoNullLists(t, "full confirm", rr, http.StatusCreated, typeOf[appInstallConfirmResponse]())
		var c appInstallConfirmResponse
		parseJSON(t, rr, &c)
		if len(c.Items) == 0 {
			t.Fatal("the full confirm made no items, so its items were not walked")
		}

		m := e.manifest(t)
		body2 := []byte(strings.Replace(string(e.files["/pack/ship.md"]), "title: Ship a change", "title: Ship a change v2", 1))
		e.files["/pack/ship.md"] = body2
		m["version"] = "1.1.0"
		m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "ship", "url": e.origin() + "/pack/ship.md", "sha256": appSHA(body2)}}
		e.publish(t, m)
		rr = doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/upgrade/preview", nil, e.token)
		assertNoNullLists(t, "changed-artifact upgrade preview", rr, http.StatusOK, typeOf[appPreview]())
		var up appPreview
		parseJSON(t, rr, &up)
		assertNoNullLists(t, "changed-artifact upgrade confirm", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/upgrade/confirm",
			map[string]any{"pending_id": up.PendingID, "manifest_sha256": up.ManifestSHA256}, e.token),
			http.StatusOK, typeOf[appUpgradeConfirmResponse]())

		state := typeOf[appInstallStateResponse]()
		assertNoNullLists(t, "install-code", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/install-code", nil, e.token), http.StatusCreated,
			typeOf[map[string]any]())
		assertNoNullLists(t, "rotate", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/rotate", nil, e.token), http.StatusOK, state)
		assertNoNullLists(t, "disable before uninstall", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/disable", nil, e.token), http.StatusOK, state)
		assertNoNullLists(t, "uninstall", doRequestWithCookie(e.srv, "POST", ws+"/apps/"+c.InstallID+"/uninstall", nil, e.token), http.StatusOK, state)
	})

	t.Run("app API and app actions", func(t *testing.T) {
		u := u11Prepare(t, appAPIFixture(t, "write"))
		f := u.appAPIFix
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
		assertNoNullLists(t, "app item update", appDo(f.srv, "PATCH", f.path("/items/"+it.ID), f.token, map[string]any{"fields_patch": map[string]any{"size": "M"}, "expected_etag": it.ETag}),
			http.StatusOK, typeOf[AppItem]())
		commentsList := typeOf[struct {
			Comments []AppComment `json:"comments"`
		}]()
		assertNoNullLists(t, "app comments (empty)", appGet(f.srv, f.path("/items/"+it.ID+"/comments"), f.token), http.StatusOK, commentsList)
		rr = appDo(f.srv, "POST", f.path("/items/"+it.ID+"/comments"), f.token, map[string]any{"body": "hi"})
		assertNoNullLists(t, "app comment create", rr, http.StatusCreated, typeOf[AppComment]())
		var cm AppComment
		parseJSON(t, rr, &cm)
		assertNoNullLists(t, "app comment update", appDo(f.srv, "PATCH", f.path("/items/"+it.ID+"/comments/"+cm.ID), f.token, map[string]any{"body": "hi again"}),
			http.StatusOK, typeOf[AppComment]())
		assertNoNullLists(t, "app comments", appGet(f.srv, f.path("/items/"+it.ID+"/comments"), f.token), http.StatusOK, commentsList)
		assertNoNullLists(t, "app items", appGet(f.srv, f.path("/collections/requests/items"), f.token), http.StatusOK,
			typeOf[struct {
				Items []AppItem `json:"items"`
			}]())
		assertNoNullLists(t, "app me", appGet(f.srv, f.path("/me"), f.token), http.StatusOK, typeOf[AppMe]())
		rr = appUpload(f, "/items/"+it.ID+"/attachments?filename=a.png", testPNG(t), int64(len(testPNG(t))))
		assertNoNullLists(t, "app upload", rr, http.StatusCreated, typeOf[appstore.AppAttachment]())
		var att appstore.AppAttachment
		parseJSON(t, rr, &att)
		assertNoNullLists(t, "app attachment", appGet(f.srv, f.path("/attachments/"+att.ID), f.token), http.StatusOK, typeOf[appstore.AppAttachment]())

		// An item action, minted and redeemed (U11).
		_, _, code, body := u.mint(t, f.item.ID, f.in.id, "open")
		if code == "" {
			t.Fatalf("mint: %s", body)
		}
		assertNoNullLists(t, "context redeem", u.redeem(code), http.StatusOK, typeOf[AppContextRedeemed]())

		// The human item-actions list: one action offered, and none.
		actions := typeOf[[]store.ItemAppAction]()
		assertNoNullLists(t, "item app actions", doRequestWithCookie(f.srv, "GET", u.itemPath(f.item.ID, "/app-actions"), nil, u.viewerTok),
			http.StatusOK, actions)
		assertNoNullLists(t, "item app actions (none)", doRequestWithCookie(f.srv, "GET", u.itemPath(f.privateItem.ID, "/app-actions"), nil, u.viewerTok),
			http.StatusOK, actions)
	})
}

// The webhook bodies are built from frozen projections, not served by a
// handler this test can call, so their census is by type: none of them has a
// slice field today. One that gains one must be added to the census above
// (and given a non-null default) before this passes.
func TestBug3417_AppEventDTOsHaveNoListFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		typeOf[store.AppItemEvent](), typeOf[store.AppItemDeletedEvent](),
		typeOf[store.AppCommentEvent](), typeOf[store.AppCommentDeletedEvent](),
	} {
		var walk func(t reflect.Type, path string)
		walk = func(rt reflect.Type, path string) {
			for rt.Kind() == reflect.Pointer {
				rt = rt.Elem()
			}
			switch rt.Kind() {
			case reflect.Slice, reflect.Array:
				t.Errorf("%s is a list field: add it to TestBug3417_AppsResponsesHaveNoNullLists", path)
			case reflect.Struct:
				for i := 0; i < rt.NumField(); i++ {
					walk(rt.Field(i).Type, path+"."+rt.Field(i).Name)
				}
			}
		}
		walk(typ, typ.Name())
	}
}
