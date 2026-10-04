package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
)

// TASK-3397 (U8a) refactors the artifact importer's preprocess into one
// function the app installer's preview and provisioning share. This test pins
// that the HUMAN import path's output is unchanged by it: every fixture is
// imported through POST /import-artifact and the stored item (title, content,
// fields) plus the response's warnings are compared against a golden captured
// from the importer BEFORE the refactor.
//
// Regenerate (only for an intended behaviour change):
//
//	PAD_UPDATE_ARTIFACT_PARITY=1 go test ./internal/server/ -run TestArtifactImportParity

const artifactParityGolden = "testdata/artifact_import_parity.json"

type parityCase struct {
	name  string
	setup func(t *testing.T, srv *Server, ws string)
	body  func(t *testing.T) []byte
}

type parityResult struct {
	Status   int            `json:"status"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Fields   map[string]any `json:"fields"`
	Warnings []string       `json:"warnings"`
}

func encodeArtifact(t *testing.T, a artifact.Artifact) []byte {
	t.Helper()
	a.FormatVersion = artifact.FormatVersion
	data, err := artifact.Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureFile(name string) func(t *testing.T) []byte {
	return func(t *testing.T) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "artifact", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
}

func artifactParityCases() []parityCase {
	return []parityCase{
		{name: "golden playbook", body: fixtureFile("playbook.golden.md")},
		{name: "golden convention", body: fixtureFile("convention.golden.md")},
		{name: "convention, active, valid selects", body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindConvention, Title: "Clean Convention",
				Fields: map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"}, Body: "A clean convention.\n"})
		}},
		{name: "convention, foreign select value", body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindConvention, Title: "Foreign",
				Fields: map[string]any{"status": "active", "trigger": "on-candidate-advance", "scope": "all"}, Body: "x\n"})
		}},
		{name: "playbook with arguments", body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindPlaybook, Title: "Playbook With Args",
				Fields: map[string]any{"status": "active", "trigger": "manual", "scope": "all", "invocation_slug": "with-args",
					"arguments": []map[string]any{{"name": "target", "type": "ref", "required": true, "description": "the thing"}}},
				Body: "Do the thing.\n"})
		}},
		{name: "playbook, slug collision", setup: func(t *testing.T, srv *Server, ws string) {
			t.Helper()
			data := encodeArtifact(t, artifact.Artifact{Kind: artifact.KindPlaybook, Title: "First",
				Fields: map[string]any{"status": "draft", "invocation_slug": "ship"}, Body: "first\n"})
			if rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data); rr.Code != http.StatusCreated {
				t.Fatalf("setup import: %d %s", rr.Code, rr.Body.String())
			}
		}, body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindPlaybook, Title: "Second",
				Fields: map[string]any{"status": "active", "invocation_slug": "ship"}, Body: "second\n"})
		}},
		{name: "with provenance", body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindConvention, Title: "Provenanced",
				Fields:     map[string]any{"status": "draft", "trigger": "on-commit"},
				Body:       "Body.\n",
				Provenance: artifact.Provenance{Workspace: "elsewhere", Author: "someone", ExportedAt: "2026-01-02T03:04:05Z"}})
		}},
		{name: "blank title", body: func(t *testing.T) []byte {
			return encodeArtifact(t, artifact.Artifact{Kind: artifact.KindConvention, Title: "   ",
				Fields: map[string]any{"status": "draft"}, Body: "x\n"})
		}},
	}
}

func runParityCase(t *testing.T, c parityCase) parityResult {
	t.Helper()
	srv := testServer(t)
	ws := createWSForTest(t, srv)
	if c.setup != nil {
		c.setup(t, srv, ws)
	}
	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", c.body(t))
	res := parityResult{Status: rr.Code}
	if rr.Code != http.StatusCreated {
		return res
	}
	var resp artifactImportResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	res.Warnings = resp.Warnings
	sort.Strings(res.Warnings)
	item := getItem(t, srv, ws, resp.Slug)
	res.Title, res.Content = item.Title, item.Content
	if err := json.Unmarshal([]byte(item.Fields), &res.Fields); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestArtifactImportParity(t *testing.T) {
	got := map[string]parityResult{}
	for _, c := range artifactParityCases() {
		got[c.name] = runParityCase(t, c)
	}
	if os.Getenv("PAD_UPDATE_ARTIFACT_PARITY") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifactParityGolden, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(artifactParityGolden)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]parityResult
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("golden has %d cases, test has %d", len(want), len(got))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("case %q missing", name)
			continue
		}
		wb, _ := json.Marshal(w)
		gb, _ := json.Marshal(g)
		if string(wb) != string(gb) {
			t.Errorf("%s changed:\nwant %s\ngot  %s", name, wb, gb)
		}
	}
}
