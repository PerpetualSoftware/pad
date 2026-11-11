package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

type linkedWriteFlags struct {
	reason, revision string
	apply, ack       bool
}

func (f *linkedWriteFlags) addFlags(c *cobra.Command) {
	c.Flags().StringVar(&f.reason, "reason", "", "why the dates change (required when moving a hard deadline)")
	c.Flags().StringVar(&f.revision, "revision", "", "opaque revision returned by the preview; repeat the previewed flags")
	c.Flags().BoolVar(&f.apply, "apply", false, "commit the change (default: preview only)")
	c.Flags().BoolVar(&f.ack, "ack-violations", false, "accept the preview's bound violations with its revision")
}

func (f linkedWriteFlags) body(body map[string]any) {
	if f.reason != "" {
		body["reason"] = f.reason
	}
	if f.revision != "" {
		body["revision"] = f.revision
	}
	if f.ack {
		body["acknowledge_violations"] = true
	}
}

type linkOffsetFlags struct {
	elapsed, clock, zone, raw string
	days                      int32
	allDay                    bool
}

func (f *linkOffsetFlags) addFlags(c *cobra.Command) {
	c.Flags().StringVar(&f.elapsed, "offset", "", "signed elapsed duration, including days (e.g. -12h, 1d6h)")
	c.Flags().Int32Var(&f.days, "days", 0, "civil day offset; selects civil mode, including zero")
	c.Flags().StringVar(&f.clock, "clock", "00:00", "civil clock HH:MM[:SS]")
	c.Flags().BoolVar(&f.allDay, "all-day", false, "civil offset produces an all-day date (clock must be zero)")
	c.Flags().StringVar(&f.zone, "zone", time.Local.String(), "civil pinned IANA zone (never Local)")
	c.Flags().StringVar(&f.raw, "offset-json", "", "API offset object, as an alternative to offset flags")
}

var linkDayRE = regexp.MustCompile(`(\d+(?:\.\d*)?|\.\d+)d`)

func (f linkOffsetFlags) body(c *cobra.Command) (map[string]any, error) {
	elapsed := c.Flags().Changed("offset")
	civil := false
	for _, k := range []string{"days", "clock", "all-day", "zone"} {
		civil = civil || c.Flags().Changed(k)
	}
	if c.Flags().Changed("offset-json") {
		if elapsed || civil {
			return nil, fmt.Errorf("--offset-json cannot be mixed with offset flags")
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(f.raw), &out); err != nil || out == nil {
			return nil, fmt.Errorf("--offset-json must be an offset object")
		}
		return out, nil
	}
	if elapsed && civil {
		return nil, fmt.Errorf("--offset cannot be mixed with civil offset flags (--days, --clock, --all-day, --zone)")
	}
	if elapsed {
		duration := linkDayRE.ReplaceAllStringFunc(f.elapsed, func(s string) string {
			// Parse the decimal with Go's integer duration parser before scaling,
			// avoiding floating-point rounding and preserving leading-dot fractions.
			hours, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
			if err != nil || hours > time.Duration(1<<63-1)/24 {
				return s
			}
			return (hours * 24).String()
		})
		d, err := time.ParseDuration(duration)
		if err != nil || d%time.Second != 0 {
			return nil, fmt.Errorf("--offset %q: want a signed duration in whole seconds (e.g. -12h or 1d6h)", f.elapsed)
		}
		return map[string]any{"mode": "elapsed", "elapsed_secs": int64(d / time.Second)}, nil
	}
	if !civil {
		return nil, nil
	}
	if !c.Flags().Changed("days") {
		return nil, fmt.Errorf("civil offset requires --days (including zero)")
	}
	if f.zone == "Local" || strings.TrimSpace(f.zone) == "" {
		return nil, fmt.Errorf("--zone must be a loadable IANA zone, never Local")
	}
	if _, err := time.LoadLocation(f.zone); err != nil {
		return nil, fmt.Errorf("--zone %q: %w", f.zone, err)
	}
	clock, err := time.Parse("15:04:05", f.clock)
	if err != nil {
		clock, err = time.Parse("15:04", f.clock)
	}
	if err != nil || clock.Nanosecond() != 0 {
		return nil, fmt.Errorf("--clock %q: want HH:MM[:SS]", f.clock)
	}
	secs := clock.Hour()*3600 + clock.Minute()*60 + clock.Second()
	if f.allDay && secs != 0 {
		return nil, fmt.Errorf("--all-day requires a zero --clock")
	}
	return map[string]any{"mode": "civil", "days": f.days, "clock_secs": secs, "all_day": f.allDay, "zone": f.zone}, nil
}

func remindLinkCmd() *cobra.Command {
	c := &cobra.Command{Use: "link", RunE: unknownSubcommandRun, Short: "Link commitment dates with an offset"}
	c.AddCommand(remindLinkWriteCmd(false), remindLinkWriteCmd(true), remindGraphReadCmd(false), remindGraphRemoveCmd(false))
	return c
}

func remindLinkWriteCmd(edit bool) *cobra.Command {
	var f linkedWriteFlags
	var offset linkOffsetFlags
	var parentRole, childRole, policy string
	use := "add <parent-ref> <child-ref>"
	n := 2
	if edit {
		use = "edit <link-id>"
		n = 1
	}
	c := &cobra.Command{Use: use, Short: "Preview or apply a commitment link", Args: cobra.ExactArgs(n), RunE: func(c *cobra.Command, args []string) error {
		off, err := offset.body(c)
		if err != nil {
			return err
		}
		if !edit && off == nil {
			return fmt.Errorf("an offset is required: --offset or --days")
		}
		if edit && off == nil && !c.Flags().Changed("cancel-policy") {
			return fmt.Errorf("at least one of offset or --cancel-policy is required")
		}
		if !edit && (parentRole == "" || childRole == "") {
			return fmt.Errorf("--parent-role and --child-role are required")
		}
		body := map[string]any{}
		if off != nil {
			body["offset"] = off
		}
		if c.Flags().Changed("cancel-policy") {
			body["cancel_policy"] = policy
		}
		if !edit {
			body["child"], body["parent_role"], body["child_role"] = args[1], parentRole, childRole
		}
		f.body(body)
		if !f.apply {
			body["preview"] = true
		}
		client, _ := getClient()
		var raw json.RawMessage
		if edit {
			raw, err = client.EditCommitmentLink(getWorkspace(), args[0], body)
		} else {
			raw, err = client.AddCommitmentLink(getWorkspace(), args[0], body)
		}
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		var result struct {
			Link  *displayCommitmentLink  `json:"link"`
			Moved models.LinkedMoveResult `json:"moved"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if result.Link != nil {
			renderCommitmentLink(os.Stdout, *result.Link)
		}
		renderLinkedMove(result.Moved, f.apply, "pad remind link "+strings.Join(argsForLinkHint(edit, args), " "))
		return nil
	}}
	c.Example = "  pad remind link add TASK-5 TASK-6 --parent-role target --child-role target --offset=-12h"
	if edit {
		c.Example = "  pad remind link edit 11111111-1111-4111-8111-111111111111 --offset 2h"
	}
	offset.addFlags(c)
	f.addFlags(c)
	c.Flags().StringVar(&policy, "cancel-policy", "", "waiting_on (server default) | cancel_child_occurrence")
	if !edit {
		c.Flags().StringVar(&parentRole, "parent-role", "", "parent date: target | earliest | deadline | occupied_until")
		c.Flags().StringVar(&childRole, "child-role", "", "child date: target | earliest | deadline | occupied_until")
	}
	return c
}

func argsForLinkHint(edit bool, args []string) []string {
	verb := "add"
	if edit {
		verb = "edit"
	}
	return append([]string{verb}, args...)
}

func remindGateCmd() *cobra.Command {
	c := &cobra.Command{Use: "gate", RunE: unknownSubcommandRun, Short: "Wait for a blocker to succeed"}
	c.AddCommand(&cobra.Command{Use: "add <ref> <blocker-ref>", Example: "  pad remind gate add TASK-5 TASK-6", Short: "Gate an item on a blocker", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		client, _ := getClient()
		raw, err := client.AddCommitmentGate(getWorkspace(), args[0], args[1])
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		var gate models.ScheduleGate
		if err = json.Unmarshal(raw, &gate); err != nil {
			return err
		}
		renderCommitmentGate(os.Stdout, gate, "")
		return nil
	}}, remindGraphReadCmd(true), remindGraphRemoveCmd(true))
	return c
}

func remindGraphReadCmd(gates bool) *cobra.Command {
	kind := "link"
	if gates {
		kind = "gate"
	}
	return &cobra.Command{Use: "list <ref>", Example: "  pad remind " + kind + " list TASK-5", Short: "List readable commitment " + kind + "s", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		client, _ := getClient()
		var raw json.RawMessage
		var err error
		if gates {
			raw, err = client.ListCommitmentGates(getWorkspace(), args[0])
		} else {
			raw, err = client.ListCommitmentLinks(getWorkspace(), args[0])
		}
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		if gates {
			var v models.ScheduleGateView
			if err = json.Unmarshal(raw, &v); err != nil {
				return err
			}
			fmt.Printf("Gates: %s\n", v.State)
			for _, g := range v.Gates {
				renderCommitmentGate(os.Stdout, g, v.State)
			}
		} else {
			var v struct {
				Links []displayCommitmentLink `json:"links"`
			}
			if err = json.Unmarshal(raw, &v); err != nil {
				return err
			}
			for _, l := range v.Links {
				renderCommitmentLink(os.Stdout, l)
			}
		}
		return nil
	}}
}

func remindGraphRemoveCmd(gates bool) *cobra.Command {
	kind := "link"
	if gates {
		kind = "gate"
	}
	return &cobra.Command{Use: "rm <" + kind + "-id>", Example: "  pad remind " + kind + " rm 11111111-1111-4111-8111-111111111111", Short: "Remove a commitment " + kind, Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		client, _ := getClient()
		var raw json.RawMessage
		var err error
		if gates {
			raw, err = client.RemoveCommitmentGate(getWorkspace(), args[0])
		} else {
			raw, err = client.RemoveCommitmentLink(getWorkspace(), args[0])
		}
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			if len(raw) == 0 {
				return nil
			}
			return cli.PrintJSON(raw)
		}
		fmt.Printf("Removed commitment %s %s\n", kind, args[0])
		return nil
	}}
}

type displayCommitmentLink struct {
	models.ScheduleLink
	ParentRef   string `json:"parent_item_ref"`
	ParentTitle string `json:"parent_item_title"`
	ChildRef    string `json:"child_item_ref"`
	ChildTitle  string `json:"child_item_title"`
}

func renderCommitmentLink(w io.Writer, l displayCommitmentLink) {
	parent, child := l.ParentItemID, l.ChildItemID
	if l.ParentRef != "" {
		parent = l.ParentRef
	}
	if l.ChildRef != "" {
		child = l.ChildRef
	}
	if l.ParentTitle != "" {
		parent += " (" + l.ParentTitle + ")"
	}
	if l.ChildTitle != "" {
		child += " (" + l.ChildTitle + ")"
	}
	off := l.Offset
	offset := fmt.Sprintf("%ds elapsed", off.ElapsedSecs)
	// Response offsets span 300 years, beyond time.Duration's range.
	if off.ElapsedSecs <= int64((1<<63-1)/int64(time.Second)) && off.ElapsedSecs >= -int64((1<<63-1)/int64(time.Second)) {
		offset = strings.TrimSuffix((time.Duration(off.ElapsedSecs) * time.Second).String(), "0m0s")
	}
	if off.Mode == "civil" {
		offset = fmt.Sprintf("%+d days at %02d:%02d:%02d %s (all_day=%t)", off.Days, off.ClockSecs/3600, off.ClockSecs/60%60, off.ClockSecs%60, off.Zone, off.AllDay)
	}
	fmt.Fprintf(w, "Link %s: %s -> %s: %s -> %s; %s; %s\n", l.ID, l.ParentRole, l.ChildRole, parent, child, offset, l.CancelPolicy)
}

func renderCommitmentGate(w io.Writer, g models.ScheduleGate, state string) {
	fmt.Fprintf(w, "Gate %s: blocker %s; %s", g.ID, g.BlockerItemID, g.Outcome)
	if state != "" {
		fmt.Fprintf(w, "; %s", state)
	}
	fmt.Fprintln(w)
}

func renderLinkedMove(m models.LinkedMoveResult, apply bool, hint string) {
	label := "Preview, nothing changed"
	if apply {
		label = "Applied"
	}
	fmt.Println(label)
	for _, group := range []struct {
		label   string
		changes []models.LinkedDateChange
	}{{"Change", m.Changes}, {"Violation", m.Violations}, {"Left in place", m.LeftInPlace}} {
		for _, ch := range group.changes {
			fmt.Printf("%s: %s %s: %s -> %s\n", group.label, ch.ItemID, ch.Role, fmtInstant(ch.From, nil), fmtInstant(ch.To, nil))
		}
	}
	if m.RenegotiatesDeadline {
		fmt.Println("Renegotiates a hard deadline (renegotiates_deadline=true).")
	}
	fmt.Printf("Revision: %s\n", m.Revision)
	if !apply {
		fmt.Printf("Apply with: %s ... --apply --revision %s (repeat the previewed flags)\n", hint, m.Revision)
	}
}

func runLinkedMove(c *cobra.Command, ref, role, to string, f linkedWriteFlags) error {
	if c.Flags().Changed("until") || c.Flags().Changed("occurrence") {
		return fmt.Errorf("--until and --occurrence cannot be used with --role; use --to")
	}
	if c.Flags().Changed("at") || c.Flags().Changed("comment") {
		return fmt.Errorf("--role uses --reason, not --at or --comment")
	}
	if to == "" {
		return fmt.Errorf("--to is required with --role")
	}
	t, err := parseInstantFlag("--to", to, false, time.Now())
	if err != nil {
		return err
	}
	body := map[string]any{"role": role, "to": t.UTC().Format(time.RFC3339)}
	if f.apply {
		f.body(body)
	} else if f.reason != "" {
		body["reason"] = f.reason
	}
	client, _ := getClient()
	raw, err := client.MoveLinkedCommitment(getWorkspace(), ref, !f.apply, body)
	if err != nil {
		return err
	}
	if formatFlag == "json" {
		return cli.PrintJSON(raw)
	}
	var m models.LinkedMoveResult
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	renderLinkedMove(m, f.apply, "pad remind move "+ref+" --role "+role+" --to "+to)
	return nil
}

func linkedResolveHint(err error, ref, role, to string) error {
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "commitment_preview_required" {
		if to == "" {
			to = "<when>"
		}
		return fmt.Errorf("%w\nPreview linked dates with: pad remind move %s --role %s --to %s", err, ref, role, to)
	}
	return staleHint(err, ref)
}
