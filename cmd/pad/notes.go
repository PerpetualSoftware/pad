package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func noteCmd() *cobra.Command {
	var details string
	var readStdin bool

	cmd := &cobra.Command{
		Use:   "note <ref> <summary>",
		Short: "Append an implementation note to an item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			body := strings.TrimSpace(details)
			if readStdin {
				body, err = readStructuredEntryBody()
				if err != nil {
					return err
				}
			}

			// Capture the entry locally before persisting so the JSON
			// branch can echo the freshly-created note back to the
			// caller (BUG-989: previously this command emitted only
			// plain text, agents had to re-fetch the item to read the
			// note ID / timestamp).
			entry := models.ItemImplementationNote{
				ID:        newStructuredEntryID("note"),
				Summary:   strings.TrimSpace(args[1]),
				Details:   body,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				// The server never parses this entry — it lives inside the
				// item's fields JSON — so the client is the last party that
				// could know who wrote it. Hardcoding "user" here made every
				// agent note claim a human wrote it (BUG-2542); leaving it
				// empty would have left it authorless, since nothing
				// downstream fills it in. Self-declared, like the header.
				CreatedBy: cli.ActorKind(),
			}
			fields, err := models.AppendImplementationNote(item.Fields, entry)
			if err != nil {
				// BUG-2675: the append refusal is retry-hostile — the stored
				// value is undecodable and every retry refuses identically.
				// Emit the structured marker so a stdio MCP agent gets that
				// code instead of a generic server_error it may retry.
				if errors.Is(err, models.ErrStructuredFieldUnreadable) {
					cli.WriteStoredStateUnreadableError(os.Stderr, err)
					// Bare error so cobra exits non-zero without re-printing
					// the (already-rendered) message.
					return fmt.Errorf("note refused: %s is unreadable on this item", models.ItemFieldImplementationNotes)
				}
				return err
			}

			updated, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
				// LastModifiedBy left empty — server-stamped (BUG-2542).
				Source: "cli",
			})
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":   cli.ItemRef(*updated),
					"title": updated.Title,
					"note":  entry,
				})
			}

			fmt.Printf("Added implementation note to %s %s\n", cli.ItemRef(*updated), updated.Title)
			return nil
		},
	}

	cmd.Flags().StringVar(&details, "details", "", "implementation details")
	cmd.Flags().BoolVar(&readStdin, "stdin", false, "read note details from stdin")
	return cmd
}

func decideCmd() *cobra.Command {
	var rationale string
	var readStdin bool

	cmd := &cobra.Command{
		Use:   "decide <ref> <decision>",
		Short: "Append a decision log entry to an item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			body := strings.TrimSpace(rationale)
			if readStdin {
				body, err = readStructuredEntryBody()
				if err != nil {
					return err
				}
			}

			// Capture the entry locally so the JSON branch can echo
			// it back without re-fetching the item (BUG-989).
			entry := models.ItemDecisionLogEntry{
				ID:        newStructuredEntryID("decision"),
				Decision:  strings.TrimSpace(args[1]),
				Rationale: body,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				// The server never parses this entry — it lives inside the
				// item's fields JSON — so the client is the last party that
				// could know who wrote it. Hardcoding "user" here made every
				// agent note claim a human wrote it (BUG-2542); leaving it
				// empty would have left it authorless, since nothing
				// downstream fills it in. Self-declared, like the header.
				CreatedBy: cli.ActorKind(),
			}
			fields, err := models.AppendDecisionLogEntry(item.Fields, entry)
			if err != nil {
				// BUG-2675 — same retry-hostile refusal as `pad item note`.
				if errors.Is(err, models.ErrStructuredFieldUnreadable) {
					cli.WriteStoredStateUnreadableError(os.Stderr, err)
					return fmt.Errorf("decision refused: %s is unreadable on this item", models.ItemFieldDecisionLog)
				}
				return err
			}

			updated, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
				// LastModifiedBy left empty — server-stamped (BUG-2542).
				Source: "cli",
			})
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"ref":      cli.ItemRef(*updated),
					"title":    updated.Title,
					"decision": entry,
				})
			}

			fmt.Printf("Added decision log entry to %s %s\n", cli.ItemRef(*updated), updated.Title)
			return nil
		},
	}

	cmd.Flags().StringVar(&rationale, "rationale", "", "decision rationale")
	cmd.Flags().BoolVar(&readStdin, "stdin", false, "read decision rationale from stdin")
	return cmd
}

func newStructuredEntryID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

func readStructuredEntryBody() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func printStructuredTimelineEntry(createdAt, createdBy, title, body string) {
	if title == "" {
		return
	}

	metaParts := make([]string, 0, 2)
	if createdAt != "" {
		if ts, err := time.Parse(time.RFC3339, createdAt); err == nil {
			metaParts = append(metaParts, cli.RelativeTime(ts))
		} else {
			metaParts = append(metaParts, createdAt)
		}
	}
	if createdBy != "" {
		metaParts = append(metaParts, createdBy)
	}

	fmt.Printf("• %s", title)
	if len(metaParts) > 0 {
		fmt.Printf("  %s", color.New(color.Faint).Sprint(strings.Join(metaParts, " · ")))
	}
	fmt.Println()
	if body != "" {
		fmt.Printf("  %s\n", body)
	}
}
