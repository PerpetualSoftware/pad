package appmanifest

import (
	"encoding/json"
	"strings"
	"testing"
)

func validManifest() map[string]any {
	return map[string]any{
		"id":            "acme/support-portal",
		"version":       "1.2.0",
		"min_contract":  map[string]any{"apps": 1, "events": 1},
		"title":         "Support Portal",
		"description":   "Requester portal",
		"publisher":     "Acme",
		"homepage":      "https://portal.acme.example/about",
		"base_url":      "https://Portal.Acme.Example:443/",
		"redirect_uris": []any{"https://portal.acme.example/oauth/callback"},
		"scopes": map[string]any{
			"service":   map[string]any{"access": "write"},
			"delegated": map[string]any{"access": "read"},
		},
		"events":      []any{map[string]any{"name": "item.created", "collections": []any{"tickets"}}},
		"webhook_url": "https://portal.acme.example/hooks/pad",
		"companion_pack": map[string]any{
			"collections": []any{map[string]any{"key": "tickets", "slug": "tickets", "name": "Tickets",
				"schema": map[string]any{"fields": []any{map[string]any{"key": "status", "label": "Status", "type": "select", "options": []any{"open", "solved"}}}}}},
			"artifacts": []any{map[string]any{"key": "triage", "url": "https://portal.acme.example/pack/triage.md", "sha256": strings.Repeat("ab", 32)}},
		},
		"item_actions":  []any{map[string]any{"key": "open", "label": "Open in portal", "collections": []any{"tickets"}, "path": "/tickets"}},
		"config_schema": map[string]any{"type": "object"},
		"docs":          "https://portal.acme.example/llms.txt",
	}
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse_Valid(t *testing.T) {
	m, err := Parse(encode(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Origin != "https://portal.acme.example" {
		t.Errorf("origin %q", m.Origin)
	}
	if len(m.CompanionPack.Collections[0].Parsed.Fields) != 1 {
		t.Errorf("schema not parsed")
	}
	if ManifestURL(m.Origin) != "https://portal.acme.example/.well-known/pad-app.json" {
		t.Errorf("manifest url %q", ManifestURL(m.Origin))
	}
}

// Each mutation breaks one rule; Parse must refuse it at the named path.
func TestParse_Refusals(t *testing.T) {
	type mut func(m map[string]any)
	pack := func(m map[string]any) map[string]any { return m["companion_pack"].(map[string]any) }
	coll := func(m map[string]any) map[string]any { return pack(m)["collections"].([]any)[0].(map[string]any) }
	art := func(m map[string]any) map[string]any { return pack(m)["artifacts"].([]any)[0].(map[string]any) }
	cases := map[string]struct {
		mut  mut
		path string
	}{
		"unknown top-level key":  {func(m map[string]any) { m["extra"] = 1 }, "$"},
		"bad id":                 {func(m map[string]any) { m["id"] = "Acme Portal" }, "id"},
		"bad version":            {func(m map[string]any) { m["version"] = "1.2" }, "version"},
		"future apps contract":   {func(m map[string]any) { m["min_contract"] = map[string]any{"apps": 2, "events": 1} }, "min_contract.apps"},
		"future events contract": {func(m map[string]any) { m["min_contract"] = map[string]any{"apps": 1, "events": 9} }, "min_contract.events"},
		"missing title":          {func(m map[string]any) { m["title"] = " " }, "title"},
		"http base_url":          {func(m map[string]any) { m["base_url"] = "http://portal.acme.example" }, "base_url"},
		"base_url with a path":   {func(m map[string]any) { m["base_url"] = "https://portal.acme.example/app" }, "base_url"},
		"base_url with userinfo": {func(m map[string]any) { m["base_url"] = "https://u:p@portal.acme.example" }, "base_url"},
		"no redirect uris":       {func(m map[string]any) { m["redirect_uris"] = []any{} }, "redirect_uris"},
		"redirect off origin":    {func(m map[string]any) { m["redirect_uris"] = []any{"https://evil.example/cb"} }, "redirect_uris[0]"},
		"redirect on other port": {func(m map[string]any) { m["redirect_uris"] = []any{"https://portal.acme.example:8443/cb"} }, "redirect_uris[0]"},
		"redirect over http":     {func(m map[string]any) { m["redirect_uris"] = []any{"http://portal.acme.example/cb"} }, "redirect_uris[0]"},
		"bad service access":     {func(m map[string]any) { m["scopes"].(map[string]any)["service"] = map[string]any{"access": "admin"} }, "scopes.service.access"},
		"webhook off origin":     {func(m map[string]any) { m["webhook_url"] = "https://evil.example/h" }, "webhook_url"},
		"events without webhook": {func(m map[string]any) { delete(m, "webhook_url") }, "webhook_url"},
		"unsubscribable event": {func(m map[string]any) {
			m["events"] = []any{map[string]any{"name": "item.bulk_updated", "collections": []any{"tickets"}}}
		}, "events[0].name"},
		"attachment event": {func(m map[string]any) {
			m["events"] = []any{map[string]any{"name": "attachment.added", "collections": []any{"tickets"}}}
		}, "events[0].name"},
		"event on unknown key": {func(m map[string]any) {
			m["events"] = []any{map[string]any{"name": "item.created", "collections": []any{"other"}}}
		}, "events[0].collections[0]"},
		"no collections":        {func(m map[string]any) { pack(m)["collections"] = []any{} }, "companion_pack.collections"},
		"bad collection slug":   {func(m map[string]any) { coll(m)["slug"] = "Tickets!" }, "companion_pack.collections[0].slug"},
		"bad collection schema": {func(m map[string]any) { coll(m)["schema"] = "not an object" }, "companion_pack.collections[0].schema"},
		"artifact off origin":   {func(m map[string]any) { art(m)["url"] = "https://cdn.example/x.md" }, "companion_pack.artifacts[0].url"},
		"artifact bad sha256":   {func(m map[string]any) { art(m)["sha256"] = "ABC" }, "companion_pack.artifacts[0].sha256"},
		"too many artifacts": {func(m map[string]any) {
			var arts []any
			for i := 0; i < MaxArtifacts+1; i++ {
				arts = append(arts, map[string]any{"key": "a" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)), "url": "https://portal.acme.example/a.md", "sha256": strings.Repeat("ab", 32)})
			}
			pack(m)["artifacts"] = arts
		}, "companion_pack.artifacts"},
		"action on unknown key": {func(m map[string]any) {
			m["item_actions"] = []any{map[string]any{"key": "o", "label": "Open", "collections": []any{"nope"}, "path": "/x"}}
		}, "item_actions[0].collections[0]"},
		"action relative path": {func(m map[string]any) {
			m["item_actions"] = []any{map[string]any{"key": "o", "label": "Open", "collections": []any{"tickets"}, "path": "x"}}
		}, "item_actions[0].path"},
		"action protocol-relative": {func(m map[string]any) {
			m["item_actions"] = []any{map[string]any{"key": "o", "label": "Open", "collections": []any{"tickets"}, "path": "//evil.example/x"}}
		}, "item_actions[0].path"},
		"action backslash path": {func(m map[string]any) {
			m["item_actions"] = []any{map[string]any{"key": "o", "label": "Open", "collections": []any{"tickets"}, "path": "/\\evil.example/x"}}
		}, "item_actions[0].path"},
		"config schema not object": {func(m map[string]any) { m["config_schema"] = []any{1} }, "config_schema"},
		"schema with a misspelled key": {func(m map[string]any) {
			coll(m)["schema"] = map[string]any{"fields": []any{map[string]any{"key": "status", "label": "Status", "type": "text", "requried": true}}}
		}, "companion_pack.collections[0].schema"},
		"homepage off origin": {func(m map[string]any) { m["homepage"] = "https://other.example/" }, "homepage"},
		"docs off origin":     {func(m map[string]any) { m["docs"] = "https://evil.example/llms.txt" }, "docs"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			c.mut(m)
			_, err := Parse(encode(t, m))
			var e *Error
			if err == nil || !IsError(err) {
				t.Fatalf("got %v, want a manifest error at %s", err, c.path)
			}
			e = err.(*Error)
			if e.Path != c.path {
				t.Fatalf("refused at %s (%s), want %s", e.Path, e.Reason, c.path)
			}
		})
	}
}

func TestParse_SizeAndTrailingData(t *testing.T) {
	if _, err := Parse(make([]byte, MaxManifestBytes+1)); !IsError(err) {
		t.Fatalf("oversize manifest: %v", err)
	}
	b := append(encode(t, validManifest()), []byte(` {}`)...)
	if _, err := Parse(b); !IsError(err) {
		t.Fatalf("trailing data: %v", err)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://EXAMPLE.com":      "https://example.com",
		"https://example.com:443/": "https://example.com",
		"https://example.com:8443": "https://example.com:8443",
		"https://[::1]:8443":       "https://[::1]:8443",
	} {
		got, err := NormalizeOrigin(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://example.com", "https://example.com/x", "https://example.com?q", "https://u@example.com", "example.com", "https://"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
