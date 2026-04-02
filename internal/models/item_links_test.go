package models

import "testing"

func TestNormalizeItemLinkType(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "default empty", raw: "", want: ItemLinkTypeRelated},
		{name: "split hyphenated", raw: "split-from", want: ItemLinkTypeSplitFrom},
		{name: "wiki hyphenated", raw: "wiki-link", want: ItemLinkTypeWikiLink},
		{name: "implements", raw: "implements", want: ItemLinkTypeImplements},
		{name: "invalid", raw: "implemented-by", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeItemLinkType(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize %q: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestIsLineageItemLinkType(t *testing.T) {
	if !IsLineageItemLinkType(ItemLinkTypeSplitFrom) {
		t.Fatal("expected split_from to be treated as lineage")
	}
	if !IsLineageItemLinkType("supersedes") {
		t.Fatal("expected supersedes to be treated as lineage")
	}
	if IsLineageItemLinkType(ItemLinkTypeBlocks) {
		t.Fatal("expected blocks to remain non-lineage")
	}
}
