package main

import (
	"encoding/base64"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// setAsideCmd reads or discards an item's set-aside edits (BUG-3244).
func setAsideCmd() *cobra.Command {
	var discard bool
	cmd := &cobra.Command{
		Use:     "set-aside <ref>",
		Short:   "Show or discard edits an editor upgrade set aside",
		Example: `  pad item set-aside DOC-3`,
		Long: `Show the edits an editor upgrade set aside on an item, or discard them.

When the collaborative editor's schema changes, edits that were typed under the
old editor and never saved to the item's body can no longer be replayed. They
are set aside instead of deleted, and the item reads
content_state=superseded_set_aside. Opening the item does NOT restore them.

With no flag this prints them as raw Yjs updates (base64 in --format json), so
they can be kept or recovered with a matching editor. --discard deletes them and
clears the state. A content write or version restore sent with
--overwrite-pending-edits discards them too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}
			if discard {
				n, err := client.DiscardCollabSetAside(ws, item.Slug)
				if err != nil {
					return err
				}
				if formatFlag == "json" {
					return cli.PrintJSON(map[string]any{"ref": item.Ref, "discarded": n})
				}
				if n == 0 {
					fmt.Printf("%s has no set-aside edits.\n", item.Ref)
					return nil
				}
				color.New(color.Faint).Printf("Discarded %d set-aside edit row(s) on %s\n", n, item.Ref)
				return nil
			}

			res, err := client.ListCollabSetAside(ws, item.Slug)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(res)
			}
			if len(res.SetAside) == 0 {
				fmt.Printf("%s has no set-aside edits.\n", item.Ref)
				return nil
			}
			fmt.Printf("%s: %d edit row(s) set aside by an editor upgrade. Opening the item will not restore them.\n\n",
				item.Ref, len(res.SetAside))
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "OP-LOG ID\tEDITOR SCHEMA\tWRITTEN\tSET ASIDE\tBYTES\tUPDATE (BASE64)\n")
			for _, r := range res.SetAside {
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\n", r.OpLogID, r.SchemaVersion,
					r.CreatedAt.UTC().Format(time.RFC3339), r.SetAsideAt.UTC().Format(time.RFC3339),
					len(r.UpdateData), base64.StdEncoding.EncodeToString(r.UpdateData))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Printf("\nDiscard them with: pad item set-aside %s --discard\n", item.Ref)
			return nil
		},
	}
	cmd.Flags().BoolVar(&discard, "discard", false, "delete the set-aside edits and clear the state")
	return cmd
}
