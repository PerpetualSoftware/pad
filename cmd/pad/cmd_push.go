package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// maxPushMessageLenForHelp mirrors server.maxPushMessageLen
// (handlers_push.go) — KEEP IN SYNC. Duplicated rather than shared
// because the two live in different packages with no existing shared
// constants package for a single value; it exists here purely so
// `pad push --help` states the bound instead of a user only learning it
// from a 400. The server is still the enforcing source of truth — this
// value is advisory (help text), not validated client-side.
const maxPushMessageLenForHelp = 4096

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
		Long: fmt.Sprintf(`pad push <ref> -m "message"
    Publish a self-addressed push notification on an item. Every one of
    your OWN connected plugin-monitor sessions (pad watch --stream
    --for-session) receives it — this is fire-and-forget over the
    in-memory watch-events bus, with no durable inbox: a push with no
    connected session listening is simply not seen (Phase 1 scope).

    -m/--message is required and must not be blank; it is the
    instruction text the receiving agent acts on (load the item first,
    then do what the message says — see the plugin skill's notification
    section for the full contract). Newlines are collapsed to spaces:
    the monitor stream is a one-line-per-event wire contract. Limited to
    %d characters after that collapse — the server rejects anything
    longer rather than truncating it.`, maxPushMessageLenForHelp),
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

	cmd.Flags().StringVarP(&message, "message", "m", "",
		fmt.Sprintf("instruction text to push (required, max %d characters after whitespace collapse)", maxPushMessageLenForHelp))
	return cmd
}
