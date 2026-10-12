package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// PLAN-3578 (lead ruling, condition 1): a rebased patch reports its
// intervening writes as complete only when the activity seq chain is
// unbroken, and a write that moves an item's seq without a stamped entry
// breaks it. Every statement that moves items.seq is classified here: STAMPED
// (its door writes a {prev_seq, seq} entry) or BREAKS (it writes none, so a
// patch rebased across it reads complete:false, which is the honest answer).
// A NEW seq-moving site fails this test until it is classified, so the
// decision is made rather than defaulted.
var seqMovingSites = map[string]string{
	"items.go:updateItemWithParentLinkOnce":               "STAMPED via updateItem (PATCH, the collab-snapshot flush, built-in update, patch). Store-direct callers (recovery flush, bulk update, internal writes) write no stamp and BREAK",
	"items.go:insertItemTx":                               "STAMPED as the chain root by createItemChecked (prev_seq 0); other creators start an item with no root, which no rebase can reach back past anyway",
	"fenced_items.go:CreateItem":                          "BREAKS: an app's item create (no stamped entry)",
	"fenced_items.go:UpdateItemFields":                    "BREAKS: an app's field write",
	"agent_roles.go:UpdateRoleSortOrder":                  "BREAKS: role-board reorder, many items at once",
	"item_sort_order.go:UpdateItemSortOrders":             "BREAKS: drag reorder, many items at once",
	"attachments.go:RemapAttachmentReferencesInWorkspace": "BREAKS: attachment reference rewrite across a workspace",
	"collections.go:applyFieldMigrationsTx":               "BREAKS: schema migration across a collection",
	"field_option_rename.go:writeRenamedArraysTx":         "BREAKS: option rename across a collection",
	"wiki_links.go:cascadeTitleRename":                    "BREAKS: wiki-link rewrite in other items on a rename",
	"export.go:importWorkspace":                           "BREAKS: workspace import creates items",
	"items.go:DeleteItem":                                 "BREAKS: archive",
	"items.go:restoreItemOnce":                            "BREAKS: restore",
	"items.go:bumpStructuralLinkSourceTx":                 "BREAKS: a link change bumps the source item",
	"items.go:moveItemWithPreCheckOnce":                   "BREAKS: collection move",
	"items_cross_workspace_copy.go:archiveItemForCopyTx":  "BREAKS: archive-source on a cross-workspace move",
	"convention_role_backfill.go:copyConventionRole":      "STAMPED: the startup role backfill (PLAN-3598) writes a system activity entry with {prev_seq, seq} in the same transaction",
}

func TestPLAN3578_EverySeqMovingSiteIsClassified(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcRe := regexp.MustCompile(`\nfunc (?:\([^)]*\) )?(\w+)`)
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for off := 0; ; {
			k := strings.Index(src[off:], "nextWorkspaceSeqSubquery")
			if k < 0 {
				break
			}
			at := off + k
			off = at + 1
			lineStart := strings.LastIndex(src[:at], "\n") + 1
			lineEnd := strings.Index(src[at:], "\n")
			if lineEnd < 0 {
				lineEnd = len(src) - at
			}
			line := src[lineStart : at+lineEnd]
			if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.HasPrefix(strings.TrimSpace(line), "const nextWorkspaceSeqSubquery") {
				continue
			}
			fns := funcRe.FindAllStringSubmatch(src[:at], -1)
			if len(fns) == 0 {
				t.Fatalf("%s: a seq-moving use outside any function", f)
			}
			found[f+":"+fns[len(fns)-1][1]] = true
		}
	}
	var unclassified, stale []string
	for k := range found {
		if _, ok := seqMovingSites[k]; !ok {
			unclassified = append(unclassified, k)
		}
	}
	for k := range seqMovingSites {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("seq-moving sites with no classification (decide: does its door write a stamped activity entry, or does it break the chain?): %v", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("classified sites that no longer move seq (remove them): %v", stale)
	}
}
