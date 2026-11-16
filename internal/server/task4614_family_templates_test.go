package server

import (
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestTASK4614_FamilyTemplatesSeedWorkspaces creates a workspace from each
// hidden family template by explicit name and checks it gets exactly the
// template's collections, system rails included.
func TestTASK4614_FamilyTemplatesSeedWorkspaces(t *testing.T) {
	cases := []struct {
		template string
		want     map[string]string // slug -> prefix ("" for the system rails)
	}{
		{"family-shared", map[string]string{
			"groceries": "GROC", "events": "EVT", "todo": "TODO", "notes": "NOTE",
			"conventions": "", "playbooks": "",
		}},
		{"family-personal", map[string]string{
			"notes": "NOTE", "todo": "TODO", "events": "EVT",
			"conventions": "", "playbooks": "",
		}},
	}
	srv := testServer(t)
	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "WS " + tc.template, "template": tc.template})
			if rr.Code != http.StatusCreated {
				t.Fatalf("create %s workspace: %d %s", tc.template, rr.Code, rr.Body.String())
			}
			var ws models.Workspace
			parseJSON(t, rr, &ws)

			rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/collections", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("list collections: %d %s", rr.Code, rr.Body.String())
			}
			var colls []models.Collection
			parseJSON(t, rr, &colls)

			var got, want []string
			for _, c := range colls {
				got = append(got, c.Slug)
				if p, ok := tc.want[c.Slug]; ok && p != "" && c.Prefix != p {
					t.Errorf("%s prefix = %q, want %q", c.Slug, c.Prefix, p)
				}
			}
			for slug := range tc.want {
				want = append(want, slug)
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s collections = %v, want %v", tc.template, got, want)
			}
		})
	}
}

// TestTASK4614_FamilyTemplatesNotListed checks the template picker endpoint
// leaves the hidden family templates out.
func TestTASK4614_FamilyTemplatesNotListed(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/templates", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list templates: %d %s", rr.Code, rr.Body.String())
	}
	var list []struct {
		Name string `json:"name"`
	}
	parseJSON(t, rr, &list)
	if len(list) == 0 {
		t.Fatal("premise: no templates listed")
	}
	for _, tmpl := range list {
		if tmpl.Name == "family-shared" || tmpl.Name == "family-personal" {
			t.Errorf("GET /api/v1/templates lists hidden template %q", tmpl.Name)
		}
	}
}
