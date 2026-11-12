package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// `pad remind` (alias `pad item remind`) and friends — the CLI half of IDEA-2641 / GitHub #1010.
//
// The verbs mirror the lifecycle rather than inventing a vocabulary: arm
// (`remind`), see (`reminders`), move (`remind --rearm`), acknowledge (`ack`),
// disarm (`unremind`).

// remindCmd is `pad remind`; itemRemindCmd is the same command under `pad item`,
// kept because `pad item remind` is what the MCP stdio door dispatches. Cobra
// cannot parent one command object twice, so each form is built fresh, with its
// own flag state.
func remindCmd() *cobra.Command     { return newRemindCmd(false) }
func itemRemindCmd() *cobra.Command { return newRemindCmd(true) }

const remindAliasNote = "This is an alias of 'pad remind'; the two are the same command.\n\n"

func newRemindCmd(alias bool) *cobra.Command {
	var (
		remindAtFlag  string
		remindRearmID string
		remindSched   scheduleFlags
	)
	cmd := &cobra.Command{
		Use:   "remind [ref]",
		Short: "Arm a reminder on an item",
		Example: `  pad remind TASK-5 --remind-at 2026-11-02T09:00:00Z
  pad remind TASK-5 --start 2026-11-09T09:00 --every weekdays
  pad remind TASK-5 --start 2026-11-14T17:00 --deadline hard`,
		Long: `Arm a one-shot reminder that fires at a specific instant.

The instant is RFC3339 and must carry a time of day — 2026-08-01T09:00:00Z, or
2026-08-01T09:00:00-04:00, which is stored as the same moment in UTC. A bare
date is refused rather than assumed to mean midnight: "2026-08-01" names a
24-hour span, and picking an hour inside it would be Pad choosing a time you
did not and then firing at it.

When the reminder fires it appears in 'pad project next' and 'pad project
ready' until you acknowledge it with 'pad item ack', and it emits an
item.reminder_due webhook event. The poll surface is not optional: an instance
with no webhook configured delivers reminders that way and only that way.

SCHEDULES. Any of --start, --every, --deadline, --window, --anchor, --audience,
--tz or --floating makes this a schedule instead: a deadline, a window, or a
rule that repeats, with one outstanding occurrence at a time. --start is
required and is a wall time with no Z or offset:

  --start     2026-11-09T09:00, or 2026-11-09 for an all-day date
  --every     daily, weekdays, weekly, biweekly, monthly, monthly-last,
              quarterly, yearly, or an RRULE such as FREQ=WEEKLY;BYDAY=MO
  --deadline  hard | soft (default is a plain check)
  --window    a span from --start: a duration (90m, 2h, 3d) or an end wall
  --anchor    calendar (default) | completion (the next one falls an interval
              after you mark it done; plain interval rules only)
  --audience  human | agent | both (default both)
  --tz        IANA zone the wall is read in; default is your account zone
  --floating  read the wall in the zone of whoever it fires for

Schedule flags cannot be combined with --remind-at or --rearm. Rules only the
server can judge (a firing floor on HOURLY and MINUTELY, anchor completion with
a BYxxx part) are refused by it, and its message is shown unchanged.

Subcommands: forecast, show, done, skip, snooze, move, renegotiate, link, gate, agenda, edit, rm (see each --help).`,
		// `[ref]` rather than `<ref>` in Use, because cmdhelp derives the
		// machine-readable arg spec from this string and `<ref>` would declare
		// a REQUIRED positional that --rearm does not take.
		// The requirement is conditional, which cmdhelp has no way to express,
		// so the honest declaration is "optional" plus the explicit check
		// below that names the two ways to call it.
		//
		// MaximumNArgs, not ExactArgs: --rearm addresses a REMINDER by id and
		// needs no item ref, so requiring one made the flag unusable. The two modes
		// are checked below rather than merged,
		// because a ref supplied alongside --rearm is ambiguous — it names an
		// item the reminder may not even belong to — and silently ignoring it
		// is how a user learns nothing about the reminder they just moved.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			if scheduleModeRequested(cmd) {
				f := remindSched
				f.RemindAt, f.Rearm = remindAtFlag, remindRearmID
				return runScheduleCreate(cmd, args, f)
			}

			if remindAtFlag == "" {
				return fmt.Errorf("--remind-at is required (an RFC3339 instant, e.g. 2026-08-01T09:00:00Z)")
			}

			if remindRearmID != "" {
				if len(args) > 0 {
					return fmt.Errorf("--rearm addresses a reminder by id, so it takes no item ref (got %q)", args[0])
				}
				r, err := client.RearmReminder(ws, remindRearmID, remindAtFlag)
				if err != nil {
					return err
				}
				if formatFlag == "json" {
					return cli.PrintJSON(r)
				}
				fmt.Printf("Re-armed reminder %s for %s\n", r.ID, r.RemindAt)
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("an item ref is required (e.g. pad item remind TASK-5 --remind-at 2026-08-01T09:00:00Z), or use --rearm <id> to move an existing reminder")
			}
			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}
			r, err := client.CreateItemReminder(ws, item.Slug, remindAtFlag)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(r)
			}
			fmt.Printf("Reminder armed on %s for %s (id %s)\n", item.Ref, r.RemindAt, r.ID)
			return nil
		},
	}
	if alias {
		cmd.Long = remindAliasNote + cmd.Long
	}
	cmd.Flags().StringVar(&remindAtFlag, "remind-at", "", "when to fire (RFC3339 instant, e.g. 2026-08-01T09:00:00Z)")
	cmd.Flags().StringVar(&remindRearmID, "rearm", "", "move an existing reminder by id instead of arming a new one")
	bindScheduleFlags(cmd, &remindSched)
	addRemindSubcommands(cmd)
	return cmd
}

func remindersCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "reminders <ref>",
		Short:   "Show an item's reminders",
		Example: `  pad item reminders TASK-5`,
		Long: `List every reminder on an item — armed, fired, and acknowledged.

Fired reminders are kept rather than deleted: the row is the record that a
reminder existed and went out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}
			reminders, err := client.ListItemReminders(ws, item.Slug)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(reminders)
			}
			if len(reminders) == 0 {
				fmt.Printf("No reminders on %s.\n", item.Ref)
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "ID\tWHEN\tSTATE\n")
			for _, r := range reminders {
				state := "armed"
				switch {
				case r.FiredAt != nil && r.AckedAt != nil:
					state = "acknowledged"
				case r.FiredAt != nil:
					state = "FIRED — needs ack"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", r.ID, r.RemindAt, state)
			}
			return w.Flush()
		},
	}
}

func ackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ack <reminder-id|ref>",
		Short: "Acknowledge a fired reminder by id or item ref",
		Example: `  pad item ack 67ec8261-505f-4ddc-8d40-fbb1393bb649
  pad item ack TASK-9`,
		Long: `Acknowledge a reminder that has fired, removing it from 'pad project next'.

Pass a reminder UUID or an item ref. An item ref acknowledges its sole fired,
unacknowledged reminder that is not managed by a schedule. If several match,
nothing is acknowledged: use a reminder id from the refusal instead.

Nothing else acknowledges a reminder. In particular, completing the item does
NOT: a reminder may have been armed precisely to fire after the work was done,
and consuming it on a status change would throw that away. A reminder on a
completed item is hidden from the recommendation surface but stays in the
table, still unacknowledged, exactly as you left it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			var r *models.Reminder
			var err error
			if _, parseErr := uuid.Parse(args[0]); parseErr == nil {
				r, err = client.AckReminder(ws, args[0])
			} else {
				r, err = client.AckItemReminder(ws, args[0])
			}
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(r)
			}
			color.New(color.Faint).Printf("Acknowledged reminder %s\n", r.ID)
			return nil
		},
	}
}

func unremindCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unremind <reminder-id>",
		Short:   "Disarm a reminder",
		Example: `  pad item unremind <reminder-id>`,
		Long: `Remove a reminder.

Deletion is the only disarm — there is no cancelled state, because a cancelled
reminder and an absent one are indistinguishable to everything that reads them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			if err := client.DeleteReminder(ws, args[0]); err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{"id": args[0], "deleted": true})
			}
			fmt.Printf("Removed reminder %s\n", args[0])
			return nil
		},
	}
}
