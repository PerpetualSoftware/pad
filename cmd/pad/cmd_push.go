package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
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
    --for-session) receives it — fire-and-forget over the watch-events
    bus, with no durable inbox: a push with no connected session
    listening is simply not seen (Phase 1 scope).

    On a multi-instance deployment (PAD_REDIS_URL set) both broadcast
    and session-targeted pushes reach your sessions on every instance:
    the notification bus and the session-presence registry are both
    shared, so a session connected to one server is visible and
    addressable from any of them. Addressable is not the same as
    delivered — a session still has to be accepting pushes, and still
    has to have access to the item — so treat the reported count as
    what was ADDRESSED, not as a receipt.

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

			result, err := client.PushItem(ws, args[0], trimmed)
			if err != nil {
				// AMBIGUOUS FAILURES GET SAID OUT LOUD (codex round 9 on
				// BUG-2699). The handler publishes BEFORE it writes its
				// response, so an error is not proof the instruction went
				// nowhere: a response lost in transit, a truncated body, a
				// gateway envelope, or the server's own push_unconfirmed all
				// reach here identically to a clean refusal. A user who reads
				// "error" and re-runs the command delivers the push twice —
				// there is no idempotency key, and the receiving agent acts
				// on each copy.
				//
				// The web dialog has drawn this distinction since TASK-2588;
				// the CLI had not, so the same outcome produced a safe
				// message in a browser and a misleading one in a terminal.
				if !pushRefusedBeforePublishing(err) {
					fmt.Fprintln(os.Stderr,
						"warning: the push may or may not have been delivered — check your agent session before re-running. "+
							"Re-sending an instruction that already landed delivers it twice.")
				}
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(result)
			}
			fmt.Printf("Pushed %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "",
		fmt.Sprintf("instruction text to push (required, max %d characters after whitespace collapse)", maxPushMessageLenForHelp))
	return cmd
}

// pushPrePublishRefusalCodes are the error codes the push endpoint can
// only produce BEFORE it publishes, so the instruction provably did not go
// out and re-running is safe.
//
// Deliberately mirrors web/src/lib/push/dispatch.ts's
// PUSH_PRE_PUBLISH_ERROR_CODES — the two surfaces answer the same question
// about the same endpoint, and letting them drift would mean a push that is
// safe to retry in a browser and unsafe in a terminal, or the reverse.
// Keep them in step.
//
// `unavailable` covers both the no-bus branch and a bus that was already
// closed; both are refusals with nothing published. `archived` is the 409
// writeItemResolveError writes when the ref names a soft-deleted item —
// item resolution happens before anything is published (codex round 12).
//
// ENUMERATED from the handler rather than taken one at a time: the codes
// handlePushToItem and its helpers can write before the publish are
// bad_request, unauthorized, unavailable, not_found (getWorkspace,
// requireItemVisible, writeItemResolveError), archived, and internal_error,
// plus the middleware codes below it. Round 12 named `archived`; the
// enumeration is what found `internal_error` alongside it.
//
// Notably ABSENT, both deliberately:
//   - push_unconfirmed, which exists precisely to say the outcome is
//     unknown.
//   - internal_error. It IS pre-publish today — this handler only reaches
//     writeInternalError from the item-resolution path — but unlike every
//     other code here that is a property of where one call sits, not of
//     what the code means. A future writeInternalError added after the
//     publish would silently make this entry wrong, and wrong in the
//     direction that costs a duplicate dispatch. A spurious warning on a
//     500 costs a sentence.
var pushPrePublishRefusalCodes = map[string]bool{
	"bad_request":         true,
	"unauthorized":        true,
	"not_found":           true,
	"forbidden":           true,
	"permission_denied":   true,
	"unavailable":         true,
	"archived":            true,
	"rate_limited":        true,
	"plan_limit_exceeded": true,
	"csrf_error":          true,
	"email_not_verified":  true,
}

// pushRefusedBeforePublishing reports whether err is a refusal the server
// provably wrote before publishing.
//
// Defaults to FALSE for anything it cannot identify — a transport error, a
// non-JSON gateway response, an unrecognised code. That default is the
// whole point: an unknown outcome must be treated as possibly-delivered,
// because the cost of being wrong in the other direction is a duplicate
// dispatch.
func pushRefusedBeforePublishing(err error) bool {
	var apiErr *cli.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return pushPrePublishRefusalCodes[apiErr.Code]
}
