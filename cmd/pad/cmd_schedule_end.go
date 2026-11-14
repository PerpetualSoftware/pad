package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// closeItemAuto is --close-item with no value: use the collection's only
// delivering close.
const closeItemAuto = "auto"

// itemStillOpenDetails is the details of a 409 item_still_open (TASK-4537).
type itemStillOpenDetails struct {
	State        string   `json:"state"`
	DoneField    string   `json:"done_field"`
	ItemStatus   string   `json:"item_status"`
	CloseOptions []string `json:"close_options"`
	AckAvailable bool     `json:"ack_available"`
}

func itemStillOpenDetailsOf(err error) (itemStillOpenDetails, bool) {
	var d itemStillOpenDetails
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "item_still_open" {
		return d, false
	}
	_ = json.Unmarshal(apiErr.Details, &d)
	if d.DoneField == "" {
		d.DoneField = "status"
	}
	return d, true
}

// hintedError prints a person-facing hint in place of the server's message
// while keeping the API error for the structured marker line.
type hintedError struct {
	msg string
	err error
}

func (e *hintedError) Error() string { return e.msg }
func (e *hintedError) Unwrap() error { return e.err }

// itemStillOpenHint says what to run next when a done or skip would end an
// open item's schedule.
func itemStillOpenHint(err error, ref, state string, d itemStillOpenDetails) error {
	status := d.ItemStatus
	if status == "" {
		status = "not set"
	}
	var ways []string
	if state == "done" {
		switch len(d.CloseOptions) {
		case 0:
		case 1:
			ways = append(ways, "--close-item to close it too")
		default:
			ways = append(ways, "--close-item=VALUE to close it too (choose one of: "+strings.Join(d.CloseOptions, ", ")+")")
		}
	}
	ways = append(ways, "--end-schedule to keep it open (it stays in Outstanding)")
	if d.AckAvailable {
		ways = append(ways, "'pad remind ack "+ref+"' to acknowledge the occurrence instead")
	}
	last := ways[len(ways)-1]
	list := last
	if len(ways) > 1 {
		list = strings.Join(ways[:len(ways)-1], ", ") + ", or " + last
	}
	return &hintedError{err: err, msg: fmt.Sprintf("%s's schedule ends with this occurrence but the item is still open (status: %s). Re-run with %s.", ref, status, list)}
}

// pickCloseValue chooses the value --close-item closes the item with: the one
// given, else the only delivering close. Nothing has been written yet.
func pickCloseValue(given, ref string, d itemStillOpenDetails) (string, error) {
	if len(d.CloseOptions) == 0 {
		return "", fmt.Errorf("%s has no delivering close value; end the schedule with --end-schedule, or close the item with pad item update. Nothing was written", ref)
	}
	// A real option wins over the bare-flag sentinel, so an option that is
	// itself named "auto" can still be chosen.
	for _, o := range d.CloseOptions {
		if strings.EqualFold(o, given) {
			return o, nil
		}
	}
	if given != closeItemAuto {
		return "", fmt.Errorf("--close-item %q is not a delivering close of %s's %s field; choose one of: %s. Nothing was written", given, ref, d.DoneField, strings.Join(d.CloseOptions, ", "))
	}
	switch len(d.CloseOptions) {
	case 1:
		return d.CloseOptions[0], nil
	}
	return "", fmt.Errorf("%s's %s field has more than one delivering close; pass --close-item=VALUE with one of: %s. Nothing was written", ref, d.DoneField, strings.Join(d.CloseOptions, ", "))
}

func remindDismissCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "dismiss <ref>",
		Short:   "Dismiss an ended schedule from Outstanding; the item stays open",
		Example: `  pad remind dismiss TASK-5`,
		Long: `Dismiss an ended schedule from Outstanding.

An open item whose schedule has ended (its last occurrence was resolved with
--end-schedule, it lapsed, or a timing edit left no occurrence) is listed in
Outstanding until the item is closed or this is run. The dismissal is shared:
it hides the entry for everyone. The item stays open and becomes ordinary
undated work. Editing the schedule's timing so it occurs again clears the
dismissal.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			raw, err := client.DismissEndedCommitment(getWorkspace(), args[0])
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(raw)
			}
			fmt.Printf("Dismissed the ended schedule of %s; the item stays open.\n", args[0])
			return nil
		},
	}
}
