package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// renegotiatePreview decodes the fields the human preview reads.
type renegotiatePreview struct {
	Preview struct {
		Scope             string  `json:"scope"`
		AppliesFrom       string  `json:"applies_from"`
		CurrentDeadline   *string `json:"current_deadline"`
		NewDeadline       *string `json:"new_deadline"`
		MissRecordedFirst bool    `json:"miss_recorded_first"`
	} `json:"preview"`
	Revision string `json:"revision"`
}

func instantOrNone(s *string) string {
	if s == nil {
		return "none"
	}
	return fmtInstant(*s, nil)
}

func remindRenegotiateCmd() *cobra.Command {
	var (
		reason, scope, deadline, behavior, revision string
		offDays, offSecs                            int
		removeDeadline, apply                       bool
	)
	c := &cobra.Command{
		Use:   "renegotiate <ref>",
		Short: "Change or remove a deadline, with a reason",
		Example: `  pad remind renegotiate TASK-5 --reason "blocked on legal" --deadline 2026-12-01T17:00:00Z
  pad remind renegotiate TASK-5 --reason "new cadence" --scope series --deadline-offset-days 2 --deadline-offset-secs 0 --apply`,
		Long: `Change the deadline of a schedule, with a reason. A hard deadline changes only
this way, and only by the item's assignee or a workspace owner.

Without --apply this is a PREVIEW: nothing is written, and the answer carries a
revision. --apply previews again and commits against that revision, so a change
that happened meanwhile is refused (409 commitment_changed) and you re-preview.
Give --revision with --apply to commit the revision you already read.

  --scope occurrence  (default) move the outstanding occurrence's deadline to
                      --deadline, an RFC3339 instant after now
  --scope series      change every occurrence from here on: --deadline-offset-days
                      and --deadline-offset-secs, --behavior lapses|stays_owed, or
                      --remove-deadline`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required: a deadline is renegotiated with a reason on record")
			}
			offsetGiven := flags.Changed("deadline-offset-days") || flags.Changed("deadline-offset-secs")
			// Which of these belongs with which scope is the server's to judge, and
			// its message is shown unchanged.
			body := map[string]any{"reason": reason, "scope": scope}
			if deadline != "" {
				t, err := time.Parse(time.RFC3339, strings.TrimSpace(deadline))
				if err != nil {
					return fmt.Errorf("--deadline %q: want an RFC3339 instant with a time of day and zone (2026-12-01T17:00:00Z)", deadline)
				}
				body["deadline"] = t.UTC().Format(time.RFC3339)
			}
			if offsetGiven {
				off := map[string]any{}
				if flags.Changed("deadline-offset-days") {
					off["deadline_days"] = offDays
				}
				if flags.Changed("deadline-offset-secs") {
					off["deadline_secs"] = offSecs
				}
				body["deadline_offset"] = off
			}
			if behavior != "" {
				body["deadline_behavior"] = behavior
			}
			if removeDeadline {
				body["remove_deadline"] = true
			}
			if revision != "" {
				body["revision"] = revision
			}

			client, _ := getClient()
			ws, ref := getWorkspace(), args[0]
			var raw json.RawMessage
			var err error
			if !apply || revision == "" {
				body["preview"] = true
				if raw, err = client.RenegotiateItemCommitment(ws, ref, body); err != nil {
					return renegotiateHint(err, ref)
				}
			}
			if apply {
				if revision == "" {
					var p renegotiatePreview
					if err := json.Unmarshal(raw, &p); err != nil || p.Revision == "" {
						return fmt.Errorf("the preview carried no revision to apply")
					}
					body["revision"] = p.Revision
				}
				body["preview"] = false
				if raw, err = client.RenegotiateItemCommitment(ws, ref, body); err != nil {
					return renegotiateHint(err, ref)
				}
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			if apply {
				v := decodeScheduleView(raw)
				noteServerTimezone(v)
				fmt.Printf("Renegotiated %s\n", v.summary(ref))
				return nil
			}
			var p renegotiatePreview
			_ = json.Unmarshal(raw, &p)
			fmt.Printf("Preview, nothing changed: %s scope, from %s\n", p.Preview.Scope, fmtInstant(p.Preview.AppliesFrom, nil))
			fmt.Printf("Deadline: %s -> %s\n", instantOrNone(p.Preview.CurrentDeadline), instantOrNone(p.Preview.NewDeadline))
			if p.Preview.MissRecordedFirst {
				fmt.Println("The outstanding occurrence is recorded as missed first.")
			}
			fmt.Printf("Revision: %s\nApply with: pad remind renegotiate %s ... --apply (add --revision %s to commit this exact preview)\n", p.Revision, ref, p.Revision)
			return nil
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "why the deadline changes; required")
	c.Flags().StringVar(&scope, "scope", "occurrence", "occurrence (this one) | series (every one from here on)")
	c.Flags().StringVar(&deadline, "deadline", "", "scope occurrence: the new deadline, an RFC3339 instant after now")
	c.Flags().IntVar(&offDays, "deadline-offset-days", 0, "scope series: days from the target to the deadline")
	c.Flags().IntVar(&offSecs, "deadline-offset-secs", 0, "scope series: seconds added to the day offset")
	c.Flags().StringVar(&behavior, "behavior", "", "scope series: lapses | stays_owed, what a missed deadline does")
	c.Flags().BoolVar(&removeDeadline, "remove-deadline", false, "scope series: drop the deadline")
	c.Flags().StringVar(&revision, "revision", "", "the revision a preview returned; with --apply, commits it without a new preview")
	c.Flags().BoolVar(&apply, "apply", false, "commit the change (default: preview only)")
	return c
}

// renegotiateHint adds the next step to the two 409s a renegotiation can meet;
// every other error is returned untouched.
func renegotiateHint(err error, ref string) error {
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "commitment_changed" {
		return fmt.Errorf("%s\nre-preview with: pad remind renegotiate %s ... (without --apply), then apply the new revision", apiErr.Message, ref)
	}
	return staleHintWith(err, ref, false)
}
