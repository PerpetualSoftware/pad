package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/bodypatch"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

// itemPatchCmd is `pad item patch` (PLAN-3578): content-anchored edits to an
// item's body, answered with the diff of exactly what changed.
func itemPatchCmd() *cobra.Command {
	var (
		olds, news  []string
		editsFlag   string
		expectedSeq int64
	)
	cmd := &cobra.Command{
		Use:   "patch <ref> (--old TEXT --new TEXT)... | --edits @FILE",
		Short: "Replace exact text in an item's body, and print the diff",
		Long: `Edit part of an item's body without sending the whole of it. Each edit
names text (--old) that must occur EXACTLY ONCE in the current body and the
text that replaces it (--new). Edits apply in order, all or nothing. The
answer is a unified diff of what changed.

Anchors are text, not line numbers: line N when you read is not line N when
the write lands. Quote enough surrounding text to be unique. Anchors
guarantee position, not meaning: if someone rewrote the paragraph around your
anchor, the edit still lands; read the diff.

--expected-seq does not refuse a moved item. If every anchor still matches,
the patch applies and reports rebased, with the writes in between and the
diff; if an anchor no longer matches, nothing is written.

For several or multi-line edits use --edits with a JSON array of
{"old","new","line_hint"} (line_hint, 1-based, only chooses among several
exact matches): --edits @edits.json, or --edits - to read stdin.

On an item open in a browser editor, that editor applies the patch to its live
document, merging with the typing there; the row catches up on its next flush.
It refuses with patch_unstable_roundtrip when applying would also change
content you did not edit, and patch_room_open when only an older editor that
cannot apply patches holds the item.`,
		Example: `  pad item patch TASK-5 --old "Status: draft" --new "Status: ready"
  pad item patch DOC-3 --old "old line" --new "new line" --expected-seq 41
  pad item patch DOC-3 --edits @edits.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			edits, err := patchEditsFromFlags(olds, news, editsFlag, os.Stdin)
			if err != nil {
				return err
			}
			client, _ := getClient()
			if !client.ServerSupportsItemPatch() {
				return fmt.Errorf("this Pad server does not support patch writes; upgrade it, or use pad item update with --expected-seq")
			}
			var seq *int64
			if cmd.Flags().Changed("expected-seq") {
				seq = &expectedSeq
			}
			res, err := client.PatchItem(getWorkspace(), args[0], edits, seq)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(res)
			}
			fmt.Printf("Patched %s (%d edit(s), seq %d)\n", res.Ref, len(edits), res.Seq)
			if res.Rebased && res.Intervening != nil {
				iv := res.Intervening
				fmt.Printf("Rebased: applied over %d write(s) since seq %d", len(iv.Changes), iv.FromSeq)
				if !iv.Complete {
					fmt.Print(" (the list is incomplete: some writes in between left no record)")
				}
				fmt.Println(". Read the diff.")
			}
			fmt.Print(res.Diff)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&olds, "old", nil, "text to replace; must occur exactly once in the current body (repeatable, paired in order with --new)")
	cmd.Flags().StringArrayVar(&news, "new", nil, "replacement text (repeatable, paired in order with --old; may be empty to delete)")
	cmd.Flags().StringVar(&editsFlag, "edits", "", `edits as a JSON array of {"old","new","line_hint"}: @FILE, - for stdin, or the JSON itself`)
	cmd.Flags().Int64Var(&expectedSeq, "expected-seq", 0, "the seq you read; a moved item is still patched when every anchor matches, and reported as rebased")
	return cmd
}

// patchEditsFromFlags builds the edit list from --old/--new pairs or --edits,
// refusing a mix, an unpaired flag, or an empty anchor before any request.
func patchEditsFromFlags(olds, news []string, editsFlag string, stdin io.Reader) ([]bodypatch.Edit, error) {
	if editsFlag != "" {
		if len(olds) > 0 || len(news) > 0 {
			return nil, fmt.Errorf("--edits conflicts with --old/--new; use one")
		}
		raw := []byte(editsFlag)
		switch {
		case editsFlag == "-":
			b, err := io.ReadAll(stdin)
			if err != nil {
				return nil, fmt.Errorf("read --edits from stdin: %w", err)
			}
			raw = b
		case strings.HasPrefix(editsFlag, "@"):
			b, err := os.ReadFile(editsFlag[1:])
			if err != nil {
				return nil, fmt.Errorf("read --edits file: %w", err)
			}
			raw = b
		}
		var edits []bodypatch.Edit
		if err := json.Unmarshal(raw, &edits); err != nil {
			return nil, fmt.Errorf(`--edits must be a JSON array of {"old","new","line_hint"}: %w`, err)
		}
		return checkEdits(edits)
	}
	if len(olds) == 0 {
		return nil, fmt.Errorf("give at least one --old/--new pair, or --edits")
	}
	if len(olds) != len(news) {
		return nil, fmt.Errorf("%d --old but %d --new: each --old needs its --new", len(olds), len(news))
	}
	edits := make([]bodypatch.Edit, len(olds))
	for i := range olds {
		edits[i] = bodypatch.Edit{Old: olds[i], New: news[i]}
	}
	return checkEdits(edits)
}

func checkEdits(edits []bodypatch.Edit) ([]bodypatch.Edit, error) {
	if len(edits) == 0 {
		return nil, fmt.Errorf("no edits")
	}
	for i, e := range edits {
		if e.Old == "" {
			return nil, fmt.Errorf("edit %d: old is empty; every edit must quote the text it replaces", i)
		}
	}
	return edits, nil
}
