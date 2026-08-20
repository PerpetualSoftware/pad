package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseCollectionTraitsEmptyForms locks the "declares nothing" cases. A
// collection with no traits is the common case, so none of these may error —
// an error here would fail bootstrap for every ordinary collection.
func TestParseCollectionTraitsEmptyForms(t *testing.T) {
	for _, raw := range []string{"", "   ", "null", "{}"} {
		got, err := ParseCollectionTraits(raw)
		if err != nil {
			t.Errorf("ParseCollectionTraits(%q) errored: %v", raw, err)
		}
		if !got.IsZero() {
			t.Errorf("ParseCollectionTraits(%q) = %+v, want zero", raw, got)
		}
	}
}

// TestParseCollectionTraitsRejectsUnknownFields is the fail-loud half
// (SPEC-0 L6). A typo in a declaration must surface AS a parse failure rather
// than as a kernel behavior that silently never fires — the latter is the
// exact failure mode traits exist to eliminate.
func TestParseCollectionTraitsRejectsUnknownFields(t *testing.T) {
	_, err := ParseCollectionTraits(`{"bootstrap_includes":[{"mode":"bodies","key":"conventions"}]}`)
	if err == nil {
		t.Fatal("misspelled trait key parsed cleanly; a typo would silently disable the behavior")
	}
	if !strings.Contains(err.Error(), "bootstrap_includes") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestCollectionTraitsValidate(t *testing.T) {
	tests := []struct {
		name    string
		traits  CollectionTraits
		wantErr string // substring; empty means must validate
	}{
		{
			name:   "zero value declares nothing",
			traits: CollectionTraits{},
		},
		{
			name: "the conventions declaration",
			traits: CollectionTraits{
				BootstrapInclude: []BootstrapInclude{
					{Mode: BootstrapModeBodies, Filter: map[string]string{"status": "active", "trigger": "always"}, Key: "conventions"},
					{Mode: BootstrapModeMetadata, Filter: map[string]string{"status": "active"}, Key: "convention_index"},
				},
				ArtifactKind: &ArtifactKindTrait{Kind: "convention"},
			},
		},
		{
			name: "the playbooks declaration",
			traits: CollectionTraits{
				BootstrapInclude: []BootstrapInclude{{Mode: BootstrapModeMetadata, Key: "playbooks"}},
				InvocationField:  InvocationSlugField,
				ArtifactKind:     &ArtifactKindTrait{Kind: "playbook"},
			},
		},
		{
			name:    "unknown mode",
			traits:  CollectionTraits{BootstrapInclude: []BootstrapInclude{{Mode: "summaries", Key: "x"}}},
			wantErr: "unknown mode",
		},
		{
			name:    "missing mode",
			traits:  CollectionTraits{BootstrapInclude: []BootstrapInclude{{Key: "x"}}},
			wantErr: "mode is required",
		},
		{
			name:    "missing key",
			traits:  CollectionTraits{BootstrapInclude: []BootstrapInclude{{Mode: BootstrapModeBodies}}},
			wantErr: "key is required",
		},
		{
			name:    "key with illegal shape",
			traits:  CollectionTraits{BootstrapInclude: []BootstrapInclude{{Mode: BootstrapModeBodies, Key: "My Key"}}},
			wantErr: "must match",
		},
		{
			// Two declarations feeding one payload from one collection is
			// always a mistake: assembly order would decide which wins.
			name: "duplicate key within a collection",
			traits: CollectionTraits{BootstrapInclude: []BootstrapInclude{
				{Mode: BootstrapModeBodies, Key: "conventions"},
				{Mode: BootstrapModeMetadata, Key: "conventions"},
			}},
			wantErr: "duplicate key",
		},
		{
			name:    "empty filter field name",
			traits:  CollectionTraits{BootstrapInclude: []BootstrapInclude{{Mode: BootstrapModeBodies, Key: "x", Filter: map[string]string{"": "active"}}}},
			wantErr: "empty field name",
		},
		{
			// SPEC-5 v1.1 amendment 4: any other field falls outside the
			// partial unique indexes that guard invocation-slug uniqueness.
			name:    "invocation_field naming another field",
			traits:  CollectionTraits{InvocationField: "route_slug"},
			wantErr: "v1 supports only",
		},
		{
			name:    "artifact_kind with no kind",
			traits:  CollectionTraits{ArtifactKind: &ArtifactKindTrait{Kind: "  "}},
			wantErr: "kind is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.traits.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestBootstrapIncludeForKey covers the lookup every consumer routes through.
func TestBootstrapIncludeForKey(t *testing.T) {
	traits := CollectionTraits{BootstrapInclude: []BootstrapInclude{
		{Mode: BootstrapModeBodies, Key: "conventions", Filter: map[string]string{"trigger": "always"}},
		{Mode: BootstrapModeMetadata, Key: "convention_index"},
	}}

	got := traits.BootstrapIncludeForKey("conventions")
	if got == nil {
		t.Fatal("BootstrapIncludeForKey(conventions) = nil")
	}
	if got.Mode != BootstrapModeBodies || got.Filter["trigger"] != "always" {
		t.Errorf("returned the wrong declaration: %+v", got)
	}
	// The second declaration on the same collection must be independently
	// reachable — the whole reason bootstrap_include is a list.
	if idx := traits.BootstrapIncludeForKey("convention_index"); idx == nil || idx.Mode != BootstrapModeMetadata {
		t.Errorf("BootstrapIncludeForKey(convention_index) = %+v, want the metadata declaration", idx)
	}
	if miss := traits.BootstrapIncludeForKey("nope"); miss != nil {
		t.Errorf("BootstrapIncludeForKey(nope) = %+v, want nil", miss)
	}
}

// TestCollectionTraitsRoundTrip guards the storage contract: what Validate
// accepts must survive a write/read cycle through the traits column.
func TestCollectionTraitsRoundTrip(t *testing.T) {
	original := CollectionTraits{
		BootstrapInclude: []BootstrapInclude{
			{Mode: BootstrapModeBodies, Filter: map[string]string{"status": "active", "trigger": "always"}, Key: "conventions"},
			{Mode: BootstrapModeMetadata, Filter: map[string]string{"status": "active"}, Key: "convention_index"},
		},
		InvocationField: InvocationSlugField,
		ArtifactKind:    &ArtifactKindTrait{Kind: "convention"},
	}

	encoded, err := original.JSON()
	if err != nil {
		t.Fatalf("JSON() errored: %v", err)
	}
	decoded, err := ParseCollectionTraits(encoded)
	if err != nil {
		t.Fatalf("ParseCollectionTraits() errored: %v", err)
	}

	a, _ := json.Marshal(original)
	b, _ := json.Marshal(decoded)
	if string(a) != string(b) {
		t.Errorf("round trip lost data:\n before: %s\n  after: %s", a, b)
	}
}

// TestZeroTraitsSerializeToEmptyObject pins the NOT NULL storage invariant:
// the column must never receive "" or "null".
func TestZeroTraitsSerializeToEmptyObject(t *testing.T) {
	got, err := CollectionTraits{}.JSON()
	if err != nil {
		t.Fatalf("JSON() errored: %v", err)
	}
	if got != "{}" {
		t.Errorf("zero traits serialized to %q, want %q", got, "{}")
	}
}

// TestParseCollectionTraitsRejectsTrailingContent closes the gap between the
// "strict parsing" claim and what json.Decoder actually does: Decode stops at
// the end of the first JSON value and ignores the rest, so a blob that is not
// valid JSON would parse cleanly and activate behavior. Codex round 7.
func TestParseCollectionTraitsRejectsTrailingContent(t *testing.T) {
	cases := []string{
		`{"artifact_kind":{"kind":"playbook"}} garbage`,
		`{"artifact_kind":{"kind":"playbook"}}{"invocation_field":"invocation_slug"}`,
		`{"invocation_field":"invocation_slug"} null`,
	}
	for _, raw := range cases {
		got, err := ParseCollectionTraits(raw)
		if err == nil {
			t.Errorf("ParseCollectionTraits(%q) accepted trailing content and returned %+v", raw, got)
		}
	}

	// Control leg: the same leading object WITHOUT trailing content parses,
	// so the rejection above is about the trailing bytes and not the object.
	if _, err := ParseCollectionTraits(`{"artifact_kind":{"kind":"playbook"}}`); err != nil {
		t.Errorf("control leg failed: the bare object should parse, got %v", err)
	}
	// Trailing WHITESPACE is not trailing content.
	if _, err := ParseCollectionTraits("{\"invocation_field\":\"invocation_slug\"}\n  \t"); err != nil {
		t.Errorf("trailing whitespace rejected: %v", err)
	}
}
