package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// ---- rendering ----

type scheduleOccurrence struct {
	At   string `json:"at"`
	Wall string `json:"wall"`
}

// scheduleView decodes the fields the table output reads. Anything else the
// server adds stays in the raw body, which --format json prints untouched.
type scheduleView struct {
	Forecast        *models.CommitmentForecast `json:"forecast"`
	Milestone       models.CommitmentMilestone `json:"milestone"`
	ItemRef         string                     `json:"item_ref"`
	Roles           models.CommitmentRoles     `json:"roles"`
	Dtstart         string                     `json:"dtstart"`
	Tz              *string                    `json:"tz"`
	Floating        bool                       `json:"floating"`
	Rrule           *string                    `json:"rrule"`
	Anchor          string                     `json:"anchor"`
	Audience        string                     `json:"audience"`
	NextOccurrence  *scheduleOccurrence        `json:"next_occurrence"`
	NextOverrideAt  *string                    `json:"next_override_at"`
	MissedCount     int                        `json:"missed_count"`
	MissedTruncated bool                       `json:"missed_truncated"`
	Stale           bool                       `json:"stale"`
	EndedAt         *string                    `json:"ended_at"`
	ErrorCode       *string                    `json:"error_code"`
	TimezoneNote    string                     `json:"timezone_note"`
	Links           []displayCommitmentLink    `json:"links"`
	Gates           models.ScheduleGateView    `json:"gates"`
}

const instantLayout = "2006-01-02 15:04 MST"

// fmtInstant renders an RFC3339 instant in the viewer's zone (loc overrides it
// when the caller named one), never the machine's.
func fmtInstant(raw string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	if loc != nil {
		return t.In(loc).Format(instantLayout)
	}
	return cli.FormatInstant(t, instantLayout)
}

// fmtWall renders a wall as "2026-11-09 09:00" (or the bare date when all-day).
func fmtWall(w string) string {
	if len(w) == len("2006-01-02") {
		return w
	}
	w = strings.Replace(w, "T", " ", 1)
	if len(w) == len("2006-01-02 15:04:05") && strings.HasSuffix(w, ":00") {
		w = w[:len(w)-3]
	}
	return w
}

func describeRule(rrule *string) string {
	if rrule == nil || *rrule == "" {
		return "once"
	}
	for _, n := range schedule.ShorthandNames() {
		if r, _ := schedule.Shorthand(n); r == *rrule {
			return n
		}
	}
	return *rrule
}

func (v scheduleView) zoneLabel() string {
	if v.Floating {
		return "floating"
	}
	if v.Tz != nil {
		return *v.Tz
	}
	return ""
}

func (v scheduleView) next() string {
	switch {
	case v.NextOccurrence == nil:
		return "none"
	case len(v.Dtstart) == len("2006-01-02") && v.NextOccurrence.Wall != "":
		return fmtWall(v.NextOccurrence.Wall)
	}
	return fmtInstant(v.NextOccurrence.At, nil)
}

func (v scheduleView) kindLabel() string {
	if v.EndedAt != nil || (v.Roles.Target == nil && v.Roles.Earliest == nil && v.Roles.Deadline == nil && v.Roles.OccupiedUntil == nil) {
		return "ended"
	}
	return schedule.CommitmentKind(v.Roles.Deadline != nil, v.Roles.OccupiedUntil != nil)
}

func (v scheduleView) summary(ref string) string {
	if v.ItemRef != "" {
		ref = v.ItemRef
	}
	s := fmt.Sprintf("%s: %s, %s from %s %s", ref, v.kindLabel(), describeRule(v.Rrule), fmtWall(v.Dtstart), v.zoneLabel())
	s = strings.TrimRight(s, " ")
	if v.Roles.OccupiedUntil != nil {
		s += " until " + fmtInstant(*v.Roles.OccupiedUntil, nil)
	}
	if v.EndedAt != nil {
		return s + "; ended"
	}
	return s + "; next " + v.next()
}

func decodeScheduleView(raw json.RawMessage) scheduleView {
	var v scheduleView
	_ = json.Unmarshal(raw, &v)
	return v
}

func noteServerTimezone(v scheduleView) {
	if v.TimezoneNote != "" && formatFlag != "json" {
		fmt.Fprintln(os.Stderr, v.TimezoneNote)
	}
}

// ---- commands ----

func runScheduleCreate(cmd *cobra.Command, args []string, f scheduleFlags) error {
	if len(args) != 1 {
		return fmt.Errorf("an item ref is required (e.g. pad item remind TASK-5 --start 2026-11-09T09:00 --every weekdays)")
	}
	body, err := buildScheduleCreate(f)
	if err != nil {
		return err
	}
	client, _ := getClient()
	raw, err := client.CreateItemCommitment(getWorkspace(), args[0], body)
	if err != nil {
		return err
	}
	if formatFlag == "json" {
		return cli.PrintJSON(raw)
	}
	v := decodeScheduleView(raw)
	noteServerTimezone(v)
	fmt.Printf("Scheduled %s\n", v.summary(args[0]))
	return nil
}

func flagChanged(cmd *cobra.Command) func(string) bool {
	return func(n string) bool { return cmd.Flags().Changed(n) }
}

func remindShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <ref>",
		Short:   "Show an item's schedule",
		Example: `  pad remind show TASK-5`,
		Long: `Show the schedule on an item: kind, rule, zone (or floating), anchor, audience, the
next occurrence in your time zone, how many occurrences passed unresolved, and
whether the schedule is active, ended or in error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			raw, err := client.GetItemCommitment(getWorkspace(), args[0])
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			v := decodeScheduleView(raw)
			noteServerTimezone(v)
			ref := v.ItemRef
			if ref == "" {
				ref = args[0]
			}
			state := "active"
			switch {
			case v.EndedAt != nil:
				state = "ended " + fmtInstant(*v.EndedAt, nil)
			case v.ErrorCode != nil:
				state = "error"
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "Item:\t%s\n", ref)
			fmt.Fprintf(w, "Kind:\t%s\n", v.kindLabel())
			fmt.Fprintf(w, "Rule:\t%s\n", describeRule(v.Rrule))
			fmt.Fprintf(w, "Starts:\t%s %s\n", fmtWall(v.Dtstart), v.zoneLabel())
			if v.Roles.OccupiedUntil != nil {
				fmt.Fprintf(w, "Ends:\t%s\n", fmtInstant(*v.Roles.OccupiedUntil, nil))
			}
			fmt.Fprintf(w, "Anchor:\t%s\n", v.Anchor)
			fmt.Fprintf(w, "Audience:\t%s\n", v.Audience)
			fmt.Fprintf(w, "Next:\t%s\n", v.next())
			if v.NextOverrideAt != nil {
				fmt.Fprintf(w, "Deferred to:\t%s\n", fmtInstant(*v.NextOverrideAt, nil))
			}
			if v.MissedCount > 0 {
				missed := strconv.Itoa(v.MissedCount)
				if v.MissedTruncated {
					missed = "at least " + missed
				}
				if v.Stale {
					missed += " (stale)"
				}
				fmt.Fprintf(w, "Missed:\t%s\n", missed)
			}
			if f := v.Forecast; f != nil {
				text := fmt.Sprintf("%s (set by %s; set at %s", f.Value, forecastSetterCLI(f), f.SetAt)
				if f.Source != "" {
					text += "; source: " + f.Source
				}
				fmt.Fprintf(w, "Forecast:\t%s)\n", text)
			}
			if m := v.Milestone; m.Total != 0 || m.Result != nil {
				result := "none"
				if m.Result != nil {
					result = *m.Result
				}
				if result == "unknown" {
					result += " (includes items you cannot see)"
				}
				fmt.Fprintf(w, "Children:\t%d of %d counted, %d waivers, result %s\n", m.Counted, m.Total, m.Waivers, result)
			}
			fmt.Fprintf(w, "State:\t%s\n", state)
			for _, link := range v.Links {
				renderCommitmentLink(w, link)
			}
			if v.Gates.State != "" {
				fmt.Fprintf(w, "Gates:\t%s\n", v.Gates.State)
			}
			for _, gate := range v.Gates.Gates {
				renderCommitmentGate(w, gate, v.Gates.State)
			}
			if v.ErrorCode != nil {
				fmt.Fprintf(w, "Error:\t%s\n", *v.ErrorCode)
			}
			return w.Flush()
		},
	}
}

// resolveVerbs maps a subcommand to the server's resolve state and the
// sentence its success prints.
type resolveVerb struct {
	use, state, short string
}

var resolveVerbs = []resolveVerb{
	{"ack", "ack", "Acknowledge the fired occurrence; keep its work outstanding"},
	{"done", "done", "Mark the outstanding occurrence done"},
	{"skip", "skip", "Skip the outstanding occurrence"},
	{"snooze", "snooze", "Re-fire the outstanding occurrence later"},
	{"move", "move", "Move the outstanding occurrence to a new time"},
}

const commentHelp = "comment recorded on the item; optional"

func remindResolveCmd(v resolveVerb) *cobra.Command {
	var comment, occurrence, until, at, role, to string
	var linked linkedWriteFlags
	needsUntil := v.state == "snooze" || v.state == "move"
	use := v.use + " <ref>"
	long := "Resolve the schedule's outstanding occurrence as " + v.state + ".\n\n" +
		"Without --occurrence the CLI reads the schedule first and resolves the occurrence it reports.\n" +
		"If that occurrence changed meanwhile (409 stale) the current one is printed."
	switch v.state {
	case "ack":
		long += "\n\nAcknowledge a fired occurrence for everyone while keeping its work outstanding. Dates and recurrence do not advance; a configured lapse still applies."
	case "done", "skip":
		long += "\n\nA recurring schedule arms its next occurrence; a one-shot ends."
		if v.state == "done" {
			long += "\n\n--at records when the work was really finished, if that was not now: an RFC3339 instant or a wall time read in your zone (2026-11-08T17:30); a bare date is refused. It cannot be in the future or before the schedule was created; the server says so unchanged."
		}
	case "snooze":
		long += "\n\n--until is an RFC3339 instant, a wall time read in your zone (2026-11-10T09:00), or a duration from now (2h, 1d). A bare date is refused. The occurrence stays outstanding and re-fires once at --until."
	case "move":
		long += "\n\n--until is an RFC3339 instant or a wall time read in your zone (2026-11-10T09:00); a bare date is refused. The occurrence stays outstanding and becomes due at --until. A move changes the target only; a hard deadline on a native schedule keeps its own date and changes through renegotiate with a reason (pad remind renegotiate). A legacy hard deadline cannot be moved: PATCH .../commitment naming a role converts it to native roles first."
	}
	if v.state == "move" {
		long += "\n\nUse --role target|earliest|deadline|occupied_until with --to WHEN to preview a linked cascade. Repeat with --apply --revision TOKEN to commit; --reason records why, and --ack-violations accepts previewed bound crossings. This form refuses --until and --occurrence."
	}
	example := "  pad remind " + v.use + " TASK-5" + map[string]string{"snooze": " --until 2h", "move": " --until 2026-11-10T09:00"}[v.state]
	if v.state != "ack" {
		example += ` -m "why"`
	}
	c := &cobra.Command{
		Use:     use,
		Short:   v.short,
		Example: example,
		Long:    long,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if v.state == "move" {
				if cmd.Flags().Changed("role") {
					return runLinkedMove(cmd, args[0], role, to, linked)
				}
				for _, flag := range []string{"to", "apply", "revision", "ack-violations", "reason"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--%s requires --role", flag)
					}
				}
			}
			req := schedule.CommitmentResolve{State: v.state, Comment: comment}
			if v.state != "ack" && cmd.Flags().Changed("at") {
				if v.state != "done" {
					return fmt.Errorf("--at records an actual completion time and applies to done only, not %s", v.state)
				}
				if untilDurationRE.MatchString(strings.TrimSpace(at)) {
					return fmt.Errorf("--at %q: a duration is not a completion time; give an RFC3339 instant or a wall time (2026-11-08T17:30)", at)
				}
				t, err := parseInstantFlag("--at", at, false, time.Now())
				if err != nil {
					return err
				}
				req.ActualDoneAt = t.UTC().Format(time.RFC3339)
			}
			if needsUntil {
				if until == "" {
					return fmt.Errorf("--until is required for %s", v.use)
				}
				t, err := parseUntil(until, v.state == "snooze", time.Now())
				if err != nil {
					return err
				}
				req.Until = t.UTC().Format(time.RFC3339)
			}
			client, _ := getClient()
			ws := getWorkspace()
			if occurrence != "" {
				parsed, err := time.Parse(time.RFC3339, occurrence)
				if err != nil {
					return fmt.Errorf("--occurrence %q: want an RFC3339 instant, as 'pad item remind show' reports it", occurrence)
				}
				if v.state == "ack" && (parsed.Nanosecond() != 0 || strings.ContainsAny(occurrence, ".,")) {
					return fmt.Errorf("--occurrence must use whole seconds")
				}
				req.OccurrenceAt = occurrence
			} else {
				raw, err := client.GetItemCommitment(ws, args[0])
				if err != nil {
					return err
				}
				sv := decodeScheduleView(raw)
				if sv.NextOccurrence == nil || sv.NextOccurrence.At == "" {
					return fmt.Errorf("%s has no outstanding occurrence to %s", args[0], v.use)
				}
				req.OccurrenceAt = sv.NextOccurrence.At
			}
			raw, err := client.ResolveItemCommitment(ws, args[0], req)
			if err != nil {
				return linkedResolveHint(err, args[0], "target", req.Until)
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			var resp struct {
				Commitment scheduleView `json:"commitment"`
			}
			_ = json.Unmarshal(raw, &resp)
			fmt.Println(resolveSentence(v.state, args[0], req, resp.Commitment))
			return nil
		},
	}
	if v.state == "move" {
		c.Flags().StringVar(&role, "role", "", "preview or move a linked date: target | earliest | deadline | occupied_until")
		c.Flags().StringVar(&to, "to", "", "linked move destination: RFC3339 or a wall time in your zone")
		linked.addFlags(c)
	}
	if v.state != "ack" {
		c.Flags().StringVarP(&comment, "comment", "m", "", commentHelp)
		c.Flags().StringVar(&at, "at", "", "when the work was really finished (done only): an RFC3339 instant or a wall time in your zone (2026-11-08T17:30)")
	}
	c.Flags().StringVar(&occurrence, "occurrence", "", "the occurrence's RFC3339 instant (default: the schedule's current one)")
	if needsUntil {
		c.Flags().StringVar(&until, "until", "", "when: an RFC3339 instant, a wall time in your zone (2026-11-10T09:00)"+map[bool]string{true: ", or a duration (2h, 1d)", false: ""}[v.state == "snooze"])
	}
	return c
}

func resolveSentence(state, ref string, req schedule.CommitmentResolve, s scheduleView) string {
	occ := fmtInstant(req.OccurrenceAt, nil)
	switch state {
	case "ack":
		return fmt.Sprintf("Acknowledged %s; this occurrence remains outstanding.", ref)
	case "snooze":
		return fmt.Sprintf("Snoozed %s until %s", ref, fmtInstant(req.Until, nil))
	case "move":
		return fmt.Sprintf("Moved %s to %s", ref, fmtInstant(req.Until, nil))
	}
	verb := map[string]string{"done": "Marked done", "skip": "Skipped"}[state]
	if req.ActualDoneAt != "" {
		occ += ", finished " + fmtInstant(req.ActualDoneAt, nil)
	}
	if s.EndedAt != nil {
		return fmt.Sprintf("%s %s (%s); the schedule has ended", verb, ref, occ)
	}
	return fmt.Sprintf("%s %s (%s); next %s", verb, ref, occ, s.next())
}

// staleHint adds the server's current occurrence to a 409 stale answer so the
// user can retry; every other error is returned untouched.
func staleHint(err error, ref string) error { return staleHintWith(err, ref, true) }

// staleHintWith is staleHint; advise adds the --occurrence retry advice, which
// only a resolve can follow.
func staleHintWith(err error, ref string, advise bool) error {
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "stale" {
		return err
	}
	var d struct {
		Ended          bool                `json:"ended"`
		NextOccurrence *scheduleOccurrence `json:"next_occurrence"`
	}
	_ = json.Unmarshal(apiErr.Details, &d)
	switch {
	case d.NextOccurrence != nil && d.NextOccurrence.At != "" && !advise:
		return fmt.Errorf("%w\ncurrent occurrence of %s: %s", err, ref, fmtInstant(d.NextOccurrence.At, nil))
	case d.NextOccurrence != nil && d.NextOccurrence.At != "":
		return fmt.Errorf("%w\ncurrent occurrence of %s: %s (pass --occurrence %s to resolve it)",
			err, ref, fmtInstant(d.NextOccurrence.At, nil), d.NextOccurrence.At)
	case d.Ended:
		return fmt.Errorf("%w\nthe schedule on %s has ended", err, ref)
	}
	return err
}

var untilDurationRE = regexp.MustCompile(`^(\d+)([mhd])$`)

// parseUntil reads --until: an RFC3339 instant, a wall in the viewer's zone, or
// (snooze) a duration from now. A bare date is refused as remind-at refuses it.
func parseUntil(s string, allowDuration bool, now time.Time) (time.Time, error) {
	return parseInstantFlag("--until", s, allowDuration, now)
}

func parseInstantFlag(flag, s string, allowDuration bool, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if m := untilDurationRE.FindStringSubmatch(s); m != nil {
		if !allowDuration {
			return time.Time{}, fmt.Errorf("%s %q: a duration works for snooze only; give an RFC3339 instant or a wall time (2026-11-10T09:00)", flag, s)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || n > 100000 {
			return time.Time{}, fmt.Errorf("%s %q is not a usable duration", flag, s)
		}
		switch m[2] {
		case "d":
			return now.In(cli.DisplayLocation()).AddDate(0, 0, n), nil
		case "h":
			return now.Add(time.Duration(n) * time.Hour), nil
		}
		return now.Add(time.Duration(n) * time.Minute), nil
	}
	if len(s) == len("2006-01-02") {
		return time.Time{}, fmt.Errorf("%s %q is a bare date; it names a 24-hour span, so give a time of day (2026-11-10T09:00)", flag, s)
	}
	if w, err := schedule.ParseWall(s); err == nil && !w.AllDay {
		return schedule.Resolve(cli.DisplayLocation(), w), nil
	}
	return time.Time{}, fmt.Errorf("%s %q: want an RFC3339 instant, a wall time in your zone (2026-11-10T09:00)%s", flag, s,
		map[bool]string{true: ", or a duration (2h, 1d)", false: ""}[allowDuration])
}

func remindEditCmd() *cobra.Command {
	var f scheduleFlags
	c := &cobra.Command{
		Use:     "edit <ref>",
		Short:   "Change an item's schedule",
		Example: `  pad remind edit TASK-5 --every weekly --start 2026-11-16T10:00`,
		Long: `Change the schedule on an item. Only the flags you give are sent.

--every "" clears the rule (one occurrence at --start). Changing --tz pins the
schedule to that zone; --floating unpins it.

The server applies its own rules to the new timing and its message is shown
unchanged when it refuses: a rule that fires too often (a floor on HOURLY and
MINUTELY rules), a rule that does not compile, --anchor completion with a
rule that is not a plain interval, or the server being busy (retry). An edit
that leaves no occurrence at or after now ends the schedule instead of failing.
A timing edit that compiles clears a stored error.
` + scheduleFlagHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			currentStart := ""
			// An end wall and a duration both need the stored start to form an offset.
			if cmd.Flags().Changed("window") && !cmd.Flags().Changed("start") && !cmd.Flags().Changed("deadline") {
				raw, err := client.GetItemCommitment(getWorkspace(), args[0])
				if err != nil {
					return err
				}
				currentStart = decodeScheduleView(raw).Dtstart
			}
			body, err := buildScheduleUpdate(f, flagChanged(cmd), currentStart)
			if err != nil {
				return err
			}
			raw, err := client.UpdateItemCommitment(getWorkspace(), args[0], body)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			v := decodeScheduleView(raw)
			noteServerTimezone(v)
			fmt.Printf("Updated %s\n", v.summary(args[0]))
			return nil
		},
	}
	bindScheduleFlags(c, &f)
	return c
}

func remindRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "rm <ref>",
		Short:   "Delete an item's schedule",
		Example: `  pad remind rm TASK-5 --yes`,
		Long: `Delete the schedule on an item.

Removed: the schedule and its rule, its history of resolved occurrences (done,
skipped, moved and missed records), and its managed reminder, so the pending
occurrence stops firing and drops out of 'pad project next' and the agenda.

Kept: the item itself, every comment already written (including those left
with -m), and any one-shot reminders armed with --remind-at.

This cannot be undone; create a new schedule with --start. Asks for
confirmation unless --yes is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				fmt.Fprintf(os.Stderr, "Delete the schedule on %s and its history? [y/N] ", args[0])
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					fmt.Fprintln(os.Stderr, "Cancelled.")
					return nil
				}
			}
			client, _ := getClient()
			raw, err := client.DeleteItemCommitment(getWorkspace(), args[0])
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			fmt.Printf("Deleted the schedule on %s\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return c
}

// ---- agenda ----

type agendaEntry struct {
	Forecast       *models.ForecastSummary `json:"forecast"`
	Type           string                  `json:"type"`
	At             string                  `json:"at"`
	Day            string                  `json:"day"`
	Date           string                  `json:"date"`
	AllDay         bool                    `json:"all_day"`
	State          string                  `json:"state"`
	AcknowledgedAt *string                 `json:"acknowledged_at"`
	AcknowledgedBy *models.AcknowledgedBy  `json:"acknowledged_by"`
	ItemRef        string                  `json:"item_ref"`
	ItemTitle      string                  `json:"item_title"`
	Roles          models.CommitmentRoles  `json:"roles"`
	Field          string                  `json:"field"`
	Overdue        bool                    `json:"overdue"`
	MissedCount    int                     `json:"missed_count"`
	Stale          bool                    `json:"stale"`
}

type agendaResponse struct {
	WindowComplete bool          `json:"window_complete"`
	TimezoneNote   string        `json:"timezone_note"`
	Entries        []agendaEntry `json:"entries"`
	Truncated      bool          `json:"truncated"`
	NextFrom       string        `json:"next_from"`
	Unexpanded     []struct {
		ItemRef  string `json:"item_ref"`
		Audience string `json:"audience"`
		Reason   string `json:"reason"`
	} `json:"unexpanded"`
	Unavailable []struct {
		ItemRef string `json:"item_ref"`
		Code    string `json:"code"`
	} `json:"unavailable"`
}

const agendaMaxDays = 92

// agendaBounds resolves --from/--days/--to against loc into the UTC instants
// sent to the server.
// clamped reports that --days reached past the server's 92x24h cap (a DST
// fall-back inside the span) and the end was pulled in to fit.
func agendaBounds(from, to string, days int, daysSet bool, loc *time.Location, now time.Time) (start, end time.Time, clamped bool, err error) {
	parse := func(flag, s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		if d, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
			return d, nil
		}
		return time.Time{}, fmt.Errorf("--%s %q: want an RFC3339 instant or a date (2026-11-09)", flag, s)
	}
	if from == "" {
		n := now.In(loc)
		start = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	} else {
		if start, err = parse("from", from); err != nil {
			return start, start, false, err
		}
	}
	switch {
	case to != "":
		if daysSet {
			return start, end, false, fmt.Errorf("--days and --to are mutually exclusive")
		}
		if end, err = parse("to", to); err != nil {
			return start, end, false, err
		}
	default:
		if days < 1 || days > agendaMaxDays {
			return start, end, false, fmt.Errorf("--days %d: the window is capped at %d days", days, agendaMaxDays)
		}
		end = start.In(loc).AddDate(0, 0, days)
		if limit := start.Add(agendaMaxDays * 24 * time.Hour); end.After(limit) {
			end, clamped = limit, true
		}
	}
	// The request carries whole seconds, so the window is whole seconds from here
	// on: what is checked, sent and compared against next_from is one value.
	start, end = start.Truncate(time.Second), end.Truncate(time.Second)
	if !end.After(start) {
		return start, end, false, fmt.Errorf("the window must end after it starts")
	}
	if end.Sub(start) > agendaMaxDays*24*time.Hour {
		return start, end, false, fmt.Errorf("the window is capped at %d days", agendaMaxDays)
	}
	return start, end, clamped, nil
}

func remindAgendaCmd() *cobra.Command {
	var from, to, tz string
	var days int
	var stale bool
	c := &cobra.Command{
		Use:   "agenda",
		Short: "List scheduled occurrences and dated items",
		Example: `  pad remind agenda
  pad remind agenda --from 2026-11-09 --days 14`,
		Long: `List the workspace's schedule occurrences and the items dated in a window, in your
time zone.

The window starts at --from (an RFC3339 instant or a date; default the start of
today in your zone) and runs --days days (default 7) or to --to. It is capped at
92 days. --tz names the zone floating schedules are read in and times are shown in.

When the list is cut short, the last line says where to continue.
The workspace JSON marker no_date_scope: "personal_only" means undated work is
not listed here; it never means "nothing undated". GET /me/agenda lists the caller's
own work in priority_list, waiting_on and no_date: assigned to the caller, or
created by the caller when nobody is assigned, for their human/agent audience.

--stale lists instead what 'pad bootstrap' collapses into "stale: K items": your
occurrences unresolved for more than 14 days, oldest first. It takes no window;
--from resumes a long list where the last line said.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := cli.DisplayLocation()
			if tz != "" {
				l, err := usertz.Validate(tz)
				if err != nil {
					return fmt.Errorf("--tz %q: %v", tz, err)
				}
				loc = l
			}
			if stale {
				return runStaleAgenda(cmd, from, to, cmd.Flags().Changed("days"), tz, loc)
			}
			start, end, clamped, err := agendaBounds(from, to, days, cmd.Flags().Changed("days"), loc, time.Now())
			if err != nil {
				return err
			}
			if clamped && formatFlag != "json" {
				fmt.Fprintf(os.Stderr, "note: the window was shortened to the server's 92-day limit; it ends %s\n", end.In(loc).Format("2006-01-02 15:04 MST"))
			}
			client, _ := getClient()
			raw, err := client.GetAgenda(getWorkspace(), start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), tz)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			var resp agendaResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("unreadable agenda answer: %w", err)
			}
			if resp.TimezoneNote != "" {
				fmt.Fprintln(os.Stderr, resp.TimezoneNote)
			}
			var showLoc *time.Location
			if tz != "" {
				showLoc = loc
			}
			renderAgenda(cmd, resp, showLoc, end, tz)
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "window start: RFC3339 instant or date (default: start of today in your zone)")
	c.Flags().IntVar(&days, "days", 7, "window length in days (max 92)")
	c.Flags().StringVar(&to, "to", "", "window end: RFC3339 instant or date (exclusive with --days)")
	c.Flags().StringVar(&tz, "tz", "", "IANA zone for floating schedules and for the times shown")
	c.Flags().BoolVar(&stale, "stale", false, "list your occurrences unresolved for more than 14 days instead (no window; --from resumes)")
	return c
}

type staleAgendaRow struct {
	State          string                 `json:"state"`
	AcknowledgedAt *string                `json:"acknowledged_at"`
	AcknowledgedBy *models.AcknowledgedBy `json:"acknowledged_by"`
	Ref            string                 `json:"ref"`
	Title          string                 `json:"title"`
	At             string                 `json:"at"`
	AllDay         bool                   `json:"all_day"`
	Roles          models.CommitmentRoles `json:"roles"`
	MissedCount    int                    `json:"missed_count"`
}

type staleAgendaAnswer struct {
	TimezoneNote string           `json:"timezone_note"`
	Entries      []staleAgendaRow `json:"entries"`
	NextFrom     string           `json:"next_from"`
	Truncated    bool             `json:"truncated"`
}

// runStaleAgenda is `agenda --stale`: no window, only --from to resume a page.
func runStaleAgenda(cmd *cobra.Command, from, to string, daysSet bool, tz string, loc *time.Location) error {
	if to != "" || daysSet {
		return errors.New("--stale takes no window: drop --to and --days; use --from to resume a long list")
	}
	var start string
	if from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			d, derr := time.ParseInLocation("2006-01-02", from, loc)
			if derr != nil {
				return fmt.Errorf("--from %q: want an RFC3339 instant or a date (2026-11-09)", from)
			}
			t = d
		}
		if t.Nanosecond() != 0 {
			return fmt.Errorf("--from %q has a fraction of a second: give a whole-second instant", from)
		}
		start = t.UTC().Format(time.RFC3339)
	}
	client, _ := getClient()
	raw, err := client.GetStaleAgenda(getWorkspace(), start, tz)
	if err != nil {
		return err
	}
	if formatFlag == "json" {
		return cli.PrintJSON(raw)
	}
	var resp staleAgendaAnswer
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("unreadable agenda answer: %w", err)
	}
	if resp.TimezoneNote != "" {
		fmt.Fprintln(os.Stderr, resp.TimezoneNote)
	}
	out := cmd.OutOrStdout()
	if len(resp.Entries) == 0 {
		if resp.Truncated {
			fmt.Fprintln(out, "list incomplete: the server read only the first 1000 schedules")
		} else {
			fmt.Fprintln(out, "Nothing has been unresolved for more than 14 days.")
		}
		return nil
	}
	var showLoc *time.Location
	if tz != "" {
		showLoc = loc
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DUE\tREF\tTITLE\tKIND\tMISSED")
	for _, e := range resp.Entries {
		due := e.At
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			if showLoc != nil {
				t = t.In(showLoc)
			} else {
				t = t.In(cli.DisplayLocation())
			}
			due = t.Format("2006-01-02 15:04 MST")
			if e.AllDay {
				due = t.Format("2006-01-02") + " all day"
			}
		}
		if e.State == "acknowledged" {
			// The last column is not tab-terminated, so the attribution there
			// does not widen or split the table's columns.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d (%s; %s)\n", due, e.Ref, strings.Join(strings.Fields(e.Title), " "), schedule.CommitmentKind(e.Roles.Deadline != nil, e.Roles.OccupiedUntil != nil), e.MissedCount, e.State, acknowledgementLine(e.AcknowledgedAt, e.AcknowledgedBy, showLoc))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", due, e.Ref, strings.Join(strings.Fields(e.Title), " "), schedule.CommitmentKind(e.Roles.Deadline != nil, e.Roles.OccupiedUntil != nil), e.MissedCount)
		}
	}
	_ = w.Flush()
	if resp.Truncated {
		fmt.Fprintln(out, "list incomplete: the server read only the first 1000 schedules")
	}
	if resp.NextFrom != "" {
		hint := "pad item remind agenda --stale --from " + resp.NextFrom
		if tz != "" {
			hint += " --tz " + tz
		}
		fmt.Fprintf(out, "more after %s: %s\n", fmtInstant(resp.NextFrom, showLoc), hint)
	}
	return nil
}

// renderAgenda prints the list. end and tz are the request's window end and
// the --tz the user gave (empty if none); the resume hint repeats them.
func renderAgenda(cmd *cobra.Command, resp agendaResponse, loc *time.Location, end time.Time, tz string) {
	out := cmd.OutOrStdout()
	if len(resp.Entries) == 0 {
		if resp.WindowComplete {
			fmt.Fprintln(out, "Nothing on the agenda in this window.")
		} else {
			fmt.Fprintln(out, "Agenda window is incomplete; no entries returned.")
		}
	} else {
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "DAY\tTIME\tSTATE\tREF\tTITLE\tKIND\tFLAGS")
		for _, e := range resp.Entries {
			day, clock := e.Day, ""
			allDay := e.AllDay || (e.Type == "date" && len(e.Date) == len("2006-01-02"))
			if t, err := time.Parse(time.RFC3339, e.At); err == nil {
				if loc != nil {
					t = t.In(loc)
				} else {
					t = t.In(cli.DisplayLocation())
				}
				if !allDay {
					day, clock = t.Format("2006-01-02"), t.Format("15:04 MST")
				}
			}
			if allDay {
				clock = "all day"
			}
			state, kind := e.State, schedule.CommitmentKind(e.Roles.Deadline != nil, e.Roles.OccupiedUntil != nil)
			if e.Type == "date" {
				state, kind = "dated", e.Field
			}
			var flags []string
			if e.Forecast != nil {
				flags = append(flags, "forecast="+e.Forecast.Value)
			}
			if e.Overdue {
				flags = append(flags, "overdue")
			}
			if e.State == "missed" || e.MissedCount > 0 {
				flags = append(flags, "missed")
			}
			if e.Stale {
				flags = append(flags, "stale")
			}
			flagText := strings.Join(flags, ",")
			if e.State == "acknowledged" {
				// In the last column, which is not tab-terminated, so it does
				// not widen or split the table's columns.
				flagText = strings.TrimPrefix(flagText+"; "+acknowledgementLine(e.AcknowledgedAt, e.AcknowledgedBy, loc), "; ")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", day, clock, state, e.ItemRef, e.ItemTitle, kind, flagText)
		}
		_ = w.Flush()
	}
	if resp.Truncated {
		// next_from at or past the window end means only schedules that could not
		// be expanded cut the list (they are named below); there is nothing to
		// resume, and a command with --from at the window end would be refused.
		if nf, err := time.Parse(time.RFC3339, resp.NextFrom); err == nil && !nf.Before(end) {
			// nothing to resume
		} else if resp.NextFrom != "" {
			hint := fmt.Sprintf("pad item remind agenda --from %s --to %s", resp.NextFrom, end.UTC().Format(time.RFC3339))
			if tz != "" {
				hint += " --tz " + tz
			}
			fmt.Fprintf(out, "more after %s: %s\n", fmtInstant(resp.NextFrom, loc), hint)
		} else {
			fmt.Fprintln(out, "list incomplete; narrow the window")
		}
	}
	for _, u := range resp.Unexpanded {
		fmt.Fprintf(out, "%s  %s  %s\n", u.ItemRef, u.Audience, u.Reason)
	}
	for _, u := range resp.Unavailable {
		fmt.Fprintf(out, "%s  unavailable  %s\n", u.ItemRef, u.Code)
	}
}

func addRemindSubcommands(cmd *cobra.Command) {
	cmd.AddCommand(remindShowCmd(), remindForecastCmd())
	for _, v := range resolveVerbs {
		cmd.AddCommand(remindResolveCmd(v))
	}
	cmd.AddCommand(remindLinkCmd(), remindGateCmd(), remindRenegotiateCmd(), remindAgendaCmd(), remindEditCmd(), remindRmCmd())
}

func acknowledgementLine(at *string, by *models.AcknowledgedBy, loc *time.Location) string {
	name, agent := "unknown user", ""
	if by != nil {
		if by.Name != "" {
			name = by.Name
		}
		if by.Actor == "agent" {
			if by.AgentName != "" {
				agent = " (" + by.AgentName + " via agent)"
			} else {
				agent = " (via agent)"
			}
		}
	}
	stamp := ""
	if at != nil {
		stamp = fmtInstant(*at, loc)
	}
	return "acknowledged by " + name + agent + " at " + stamp
}
