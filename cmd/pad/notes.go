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

			body := strings.TrimSpace(details)
			if readStdin {
				var err error
				body, err = readStructuredEntryBody()
				if err != nil {
					return err
				}
			}
			summary := strings.TrimSpace(args[1])

			var (
				updated *models.Item
				entry   models.ItemImplementationNote
				err     error
			)
			if client.ServerSupportsItemFieldAppend() {
				// BUG-3056: the server appends under its write lock, so a
				// concurrent write to another key cannot be reverted by this
				// one. It mints the id, timestamp and attribution.
				updated, err = client.UpdateItem(ws, args[0], models.ItemUpdate{
					AppendImplementationNote: &models.ItemImplementationNoteAppend{Summary: summary, Details: body},
					Source:                   "cli",
				})
				if err != nil {
					return structuredAppendRefusal(err, "note", models.ItemFieldImplementationNotes)
				}
				if updated.Appended == nil || updated.Appended.ImplementationNote == nil ||
					!implementationNotePresent(updated.Fields, updated.Appended.ImplementationNote.ID) {
					return unconfirmedAppendError("note", cli.ItemRef(*updated))
				}
				entry = *updated.Appended.ImplementationNote
			} else {
				updated, entry, err = legacyAppendNote(client, ws, args[0], summary, body)
				if err != nil {
					return err
				}
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

			body := strings.TrimSpace(rationale)
			if readStdin {
				var err error
				body, err = readStructuredEntryBody()
				if err != nil {
					return err
				}
			}
			decision := strings.TrimSpace(args[1])

			var (
				updated *models.Item
				entry   models.ItemDecisionLogEntry
				err     error
			)
			if client.ServerSupportsItemFieldAppend() {
				// BUG-3056 — see `pad item note`.
				updated, err = client.UpdateItem(ws, args[0], models.ItemUpdate{
					AppendDecision: &models.ItemDecisionLogAppend{Decision: decision, Rationale: body},
					Source:         "cli",
				})
				if err != nil {
					return structuredAppendRefusal(err, "decision", models.ItemFieldDecisionLog)
				}
				if updated.Appended == nil || updated.Appended.Decision == nil ||
					!decisionPresent(updated.Fields, updated.Appended.Decision.ID) {
					return unconfirmedAppendError("decision", cli.ItemRef(*updated))
				}
				entry = *updated.Appended.Decision
			} else {
				updated, entry, err = legacyAppendDecision(client, ws, args[0], decision, body)
				if err != nil {
					return err
				}
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

// legacyAppendNote is `pad item note` against a server that does not
// advertise item_field_append: GET, append locally, PATCH the whole fields
// blob back. It carries the BUG-3056 race (a concurrent write to another key
// between the GET and the PATCH is reverted), which is the price of talking
// to that server at all; it is still correct in every other respect.
func legacyAppendNote(client *cli.Client, ws, ref, summary, body string) (*models.Item, models.ItemImplementationNote, error) {
	item, err := client.GetItem(ws, ref)
	if err != nil {
		return nil, models.ItemImplementationNote{}, err
	}
	entry := models.ItemImplementationNote{
		ID:        models.NewStructuredEntryID("note"),
		Summary:   summary,
		Details:   body,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		// On this path the client writes the entry whole, so it is the last
		// party that could know who wrote it (BUG-2542). Self-declared, like
		// the X-Pad-Agent header.
		CreatedBy: cli.ActorKind(),
	}
	fields, err := models.AppendImplementationNote(item.Fields, entry)
	if err != nil {
		return nil, entry, structuredAppendRefusal(err, "note", models.ItemFieldImplementationNotes)
	}
	updated, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{
		Fields: &fields,
		// LastModifiedBy left empty — server-stamped (BUG-2542).
		Source: "cli",
	})
	return updated, entry, err
}

// legacyAppendDecision is legacyAppendNote for `pad item decide`.
func legacyAppendDecision(client *cli.Client, ws, ref, decision, body string) (*models.Item, models.ItemDecisionLogEntry, error) {
	item, err := client.GetItem(ws, ref)
	if err != nil {
		return nil, models.ItemDecisionLogEntry{}, err
	}
	entry := models.ItemDecisionLogEntry{
		ID:        models.NewStructuredEntryID("decision"),
		Decision:  decision,
		Rationale: body,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		CreatedBy: cli.ActorKind(),
	}
	fields, err := models.AppendDecisionLogEntry(item.Fields, entry)
	if err != nil {
		return nil, entry, structuredAppendRefusal(err, "decision", models.ItemFieldDecisionLog)
	}
	updated, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{
		Fields: &fields,
		Source: "cli",
	})
	return updated, entry, err
}

// structuredAppendRefusal turns the Append* helpers' refusal — raised locally
// on the legacy path, or by the server as 409 stored_state_unreadable on the
// append path — into the structured marker a stdio MCP agent reads as the
// retry-hostile code rather than a generic server_error (BUG-2675). Any other
// error is returned unchanged.
func structuredAppendRefusal(err error, kind, key string) error {
	refusal := err
	var apiErr *cli.APIError
	switch {
	case errors.Is(err, models.ErrStructuredFieldUnreadable):
	case errors.As(err, &apiErr) && apiErr.Code == cli.StoredStateUnreadableCode:
		refusal = errors.New(apiErr.Message)
	default:
		return err
	}
	cli.WriteStoredStateUnreadableError(os.Stderr, refusal)
	// Bare error so cobra exits non-zero without re-printing the
	// already-rendered message.
	return fmt.Errorf("%s refused: %s is unreadable on this item", kind, key)
}

// unconfirmedAppendError is the append path's post-write assertion failing:
// the server accepted the request but its response does not show the entry.
// It is reported, and deliberately NOT followed by a legacy write — the
// capability check already said this server appends, so a second write would
// duplicate the entry whenever it did (lead ruling on BUG-3056).
func unconfirmedAppendError(kind, ref string) error {
	return fmt.Errorf("the server accepted the %s for %s but its response does not show it appended; "+
		"it was NOT re-sent, so check `pad item show %s --format json` before trying again", kind, ref, ref)
}

func implementationNotePresent(fieldsJSON, id string) bool {
	for _, n := range models.ExtractItemImplementationNotes(fieldsJSON) {
		if n.ID == id {
			return true
		}
	}
	return false
}

func decisionPresent(fieldsJSON, id string) bool {
	for _, d := range models.ExtractItemDecisionLog(fieldsJSON) {
		if d.ID == id {
			return true
		}
	}
	return false
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
