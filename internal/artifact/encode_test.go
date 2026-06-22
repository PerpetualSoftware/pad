package artifact

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

func samplePlaybook() Artifact {
	return Artifact{
		Kind:          KindPlaybook,
		FormatVersion: FormatVersion,
		Title:         "Ship a change",
		Fields: map[string]any{
			"status":          "active",
			"trigger":         "on-intent",
			"scope":           "workspace",
			"invocation_slug": "ship",
			"arguments": []map[string]any{
				{"name": "ref", "type": "ref", "required": true, "description": "the item to ship"},
				{"name": "merge-strategy", "type": "enum", "required": false, "default": "squash"},
			},
		},
		Body: "## Steps\n\n1. Run the tests\n2. Open a PR\n",
		Provenance: Provenance{
			Workspace:     "demo",
			ExportedAt:    "2026-06-22T00:00:00Z",
			Author:        "xarmian",
			FormatVersion: FormatVersion,
		},
	}
}

func sampleConvention() Artifact {
	return Artifact{
		Kind:          KindConvention,
		FormatVersion: FormatVersion,
		Title:         "Run the test suite before pushing",
		Fields: map[string]any{
			"status":   "active",
			"trigger":  "on-commit",
			"scope":    "repo",
			"priority": "high",
			"role":     "engineer",
		},
		Body: "Always run `make test` before pushing to main.\n",
		Provenance: Provenance{
			Workspace:     "demo",
			ExportedAt:    "2026-06-22T00:00:00Z",
			Author:        "xarmian",
			FormatVersion: FormatVersion,
		},
	}
}

func TestEncodeGolden(t *testing.T) {
	cases := []struct {
		name     string
		artifact Artifact
		golden   string
	}{
		{"playbook", samplePlaybook(), "playbook.golden.md"},
		{"convention", sampleConvention(), "convention.golden.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.artifact)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Encode output mismatch with %s\n--- got ---\n%s\n--- want ---\n%s", tc.golden, got, want)
			}
		})
	}
}

func TestEncodeDeterministic(t *testing.T) {
	for _, a := range []Artifact{samplePlaybook(), sampleConvention()} {
		first, err := Encode(a)
		if err != nil {
			t.Fatalf("Encode 1: %v", err)
		}
		second, err := Encode(a)
		if err != nil {
			t.Fatalf("Encode 2: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("Encode not deterministic for %s:\n%s\nvs\n%s", a.Kind, first, second)
		}
	}
}

func TestEncodeUnknownKind(t *testing.T) {
	_, err := Encode(Artifact{Kind: "nonsense"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestEncodeDefaultsVersion(t *testing.T) {
	a := samplePlaybook()
	a.FormatVersion = 0
	a.Provenance.FormatVersion = 0
	out, err := Encode(a)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(out, []byte("format_version: 1")) {
		t.Errorf("expected default format_version: 1 in output:\n%s", out)
	}
}
