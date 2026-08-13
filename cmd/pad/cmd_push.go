package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// pushCmd is `pad push <ref> -m "message"` (IDEA-2544 Phase 1) — the
// "push this to my agent" verb: an explicit, user-authored instruction
// bound to an item, delivered to the caller's own connected monitor
// sessions over the same watch-events stream `pad watch --stream
// --for-session` consumes. Distinct from `pad watch` (a durable, item-
// scoped subscription): a push is one-shot, transient, and IS an
// instruction rather than a passive fact — see the plugin skill's
// notification-etiquette section for the receiving-agent behavior
// contract.
func pushCmd() *cobra.Command {
	var message string

	cmd := &cobra.Command{
		Use:   "push <ref>",
		Short: "Push an item + instruction to your own connected agent session(s)",
		Long: `pad push <ref> -m "message"
    Publish a self-addressed push notification on an item. Every one of
    your OWN connected plugin-monitor sessions (pad watch --stream
    --for-session) receives it — this is fire-and-forget over the
    in-memory watch-events bus, with no durable inbox: a push with no
    connected session listening is simply not seen (Phase 1 scope).

    -m/--message is required and must not be blank; it is the
    instruction text the receiving agent acts on (load the item first,
    then do what the message says — see the plugin skill's notification
    section for the full contract). Newlines are collapsed to spaces:
    the monitor stream is a one-line-per-event wire contract.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			trimmed := strings.Join(strings.Fields(message), " ")
			if trimmed == "" {
				return fmt.Errorf("-m/--message is required and must not be blank")
			}

			client, _ := getClient()
			ws := getWorkspace()

			if err := client.PushItem(ws, args[0], trimmed); err != nil {
				return err
			}

			fmt.Printf("Pushed %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "instruction text to push (required)")
	return cmd
}
