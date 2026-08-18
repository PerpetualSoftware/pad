package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

// noPadTomlRetryInterval is how long `pad watch --stream --for-session`
// sleeps before re-checking for a .pad.toml when none was found in the
// directory tree — DOC-2479's silent-start contract: "no .pad.toml in
// tree → sleep-retry hourly, print nothing".
const noPadTomlRetryInterval = time.Hour

// padddBackoffBase / padddBackoffCap bound the retry delay when padd is
// unreachable (connection refused, non-200, or the stream drops
// mid-read). DOC-2479 distinguishes this from the no-workspace case
// above ("padd unreachable → backoff retry, print nothing") — a
// persistently-down padd shouldn't hot-loop, but shouldn't make the
// caller wait a full hour either, unlike the no-.pad.toml case where an
// hour is the right cadence for "nothing to do until a workspace shows
// up".
const (
	padddBackoffBase = 5 * time.Second
	padddBackoffCap  = 5 * time.Minute
)

// padddBackoff computes the delay before the Nth reconnect attempt
// (attempt=1 is the first retry after an initial failed connection).
// Linear and capped — extracted as a pure function so the retry math is
// unit-testable without a real clock (per the dispatcher's ask: "unit-
// test the decision logic, don't literally sleep").
func padddBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(attempt) * padddBackoffBase
	if d > padddBackoffCap {
		d = padddBackoffCap
	}
	return d
}

// watchStreamPayload mirrors server.watchEventPayload — the wire shape
// of GET /api/v1/events/stream's "notification" events (DOC-2479):
// {id, ts, workspace, item_ref, kind, actor, summary}.
type watchStreamPayload struct {
	ID        int64  `json:"id"`
	Ts        int64  `json:"ts"`
	Workspace string `json:"workspace"`
	ItemRef   string `json:"item_ref"`
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	Summary   string `json:"summary"`
}

// formatMonitorLine renders one notification as the exact stdout-line
// contract `pad watch --stream --for-session` promises (DOC-2479):
// "PAD demo/TASK-214 → done (Dave): fix verified" — one line, facts
// only, no etiquette prose (that lives in the plugin skill per DR-4).
//
// TASK-2533 plan interpretation: DOC-2479's example names a target
// STATUS VALUE and reads as though it combines two facts (a status
// change AND an attached comment) into one line. This system publishes
// one notification per fact (see server.publishWatchNotifications'
// producer-coverage audit) rather than inventing a combining mechanism
// DOC-2479 doesn't specify a wire shape for, so a
// `pad item update --field status=done --comment "..."` call surfaces
// as TWO lines here, and this formatter uses the notification's kind
// (not a status value) as the arrow's target — it's the one thing every
// kind (status-change / assignment / comment / the reserved ask) can
// supply uniformly from the actual payload fields.
//
// The workspace slug prefix (IDEA-2544 Phase 1, dispatcher review round
// 2, codex P1) is UNIVERSAL — every kind, not push-specific — because
// the ambiguity it closes predates push: GET /api/v1/events/stream is
// user-scoped ACROSS every workspace the caller belongs to (a watch is
// personal, not workspace-scoped — see Store.ListWatchesForUser), so
// ANY kind can arrive for a workspace other than the one linked in the
// caller's cwd, not just push. The payload already carried Workspace
// (DOC-2479's wire contract); it was simply never rendered. Dropping it
// silently means a session linked to workspace A that receives a
// notification for workspace B resolves the wrong item (or 404s) with
// no signal in the line that anything was off. Safe to change
// universally: formatMonitorLine's only consumer is this file's own
// fmt.Println (grepped plugin/ and skills/ for anything else parsing
// "PAD ..." lines — there is none; the Claude Code plugin host ingests
// the stdout line as free-text session-notification prose, not a
// structured format any code parses), so there is no wire-format
// consumer to break by adding a field.
func formatMonitorLine(p watchStreamPayload) string {
	return fmt.Sprintf("PAD %s/%s → %s (%s): %s", p.Workspace, p.ItemRef, p.Kind, p.Actor, p.Summary)
}

// sleepOrDone waits for d or ctx cancellation, whichever comes first.
// Returns false if ctx was cancelled — the caller should stop looping,
// not schedule another retry, when that happens (Ctrl+C / SIGTERM
// during a silent-retry wait must exit promptly, not after the full
// interval).
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// watchCmdGroup is `pad watch` (TASK-2533). Named ...Group, not watchCmd,
// because that identifier is already taken by `pad project watch`
// (cmd_project.go) — a different, pre-existing command that streams a
// whole workspace's activity feed for a human terminal. This command is
// per-item and user-scoped; the two are unrelated beyond sharing the
// English verb "watch".
func watchCmdGroup() *cobra.Command {
	var untilPredicate string
	var stream bool
	var forSession bool

	cmd := &cobra.Command{
		Use:   "watch [ref]",
		Short: "Create a durable watch on an item, or run the session-monitor stream",
		Long: `pad watch <ref>
    Create (or update) a durable, server-side watch on an item. Watches
    survive monitor restarts and padd restarts — they are not client-side
    state.

pad watch <ref> --until status=X
    Same, but the item only notifies once its status-equivalent field
    reaches X (an unconditional watch, the default, notifies on every
    status-change / assignment / comment on the item).

pad watch list
    List your active watches, across every workspace you belong to.

pad watch remove <ref>
    Stop watching an item.

pad watch --stream --for-session
    The plugin monitor command (see monitors/monitors.json). Prints one
    line per matching event to stdout in a fixed, machine-readable
    format. Silent on startup when no workspace is linked or padd is
    unreachable — see the DOC-2479 behavior contract in this command's
    source. Not meant to be run by hand; the pad plugin's monitor entry
    invokes it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stream || forSession {
				if !stream || !forSession {
					return fmt.Errorf("--stream and --for-session must be used together")
				}
				if len(args) > 0 {
					return fmt.Errorf("--stream --for-session does not take a ref argument")
				}
				return runWatchMonitor(cmd.Context())
			}
			if len(args) != 1 {
				return cmd.Help()
			}
			return runCreateWatch(args[0], untilPredicate)
		},
	}

	cmd.Flags().StringVar(&untilPredicate, "until", "", `optional predicate, e.g. --until status=done`)
	cmd.Flags().BoolVar(&stream, "stream", false, "run the session-monitor stream (requires --for-session)")
	cmd.Flags().BoolVar(&forSession, "for-session", false, "format output for the Claude Code plugin monitor (requires --stream)")

	cmd.AddCommand(watchListCmd(), watchRemoveCmd())
	return cmd
}

func runCreateWatch(ref, predicate string) error {
	client, _ := getClient()
	ws := getWorkspace()

	w, err := client.CreateWatch(ws, ref, predicate)
	if err != nil {
		return err
	}

	if formatFlag == "json" {
		return cli.PrintJSON(w)
	}
	if predicate != "" {
		fmt.Printf("Watching %s until %s\n", w.ItemRef, predicate)
	} else {
		fmt.Printf("Watching %s\n", w.ItemRef)
	}
	return nil
}

func watchListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your active watches",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			watches, err := client.ListWatches()
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(watches)
			}
			if len(watches) == 0 {
				fmt.Println("No active watches.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "REF\tWORKSPACE\tTITLE\tPREDICATE")
			for _, w := range watches {
				predicate := w.Predicate
				if predicate == "" {
					predicate = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.ItemRef, w.WorkspaceSlug, w.ItemTitle, predicate)
			}
			return tw.Flush()
		},
	}
}

func watchRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <ref>",
		Short: "Stop watching an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			if err := client.DeleteWatch(ws, args[0]); err != nil {
				return err
			}
			fmt.Printf("Stopped watching %s\n", args[0])
			return nil
		},
	}
}

// monitorClient builds a Client for the monitor loop WITHOUT any of
// getClient()'s side effects that are fatal (or interactive) for an
// unattended background process (codex round 1 finding 5):
// getConfiguredConfig() calls os.Exit(1) when the client isn't
// configured and stdin isn't a TTY, or — worse, when a TTY IS attached —
// launches an INTERACTIVE configuration wizard that blocks on stdin and
// prints prose to stdout. Either behavior directly violates DOC-2479's
// silent-start contract, which requires "not ready yet" to be a silent
// retry, never a crash or a prompt.
//
// This reads config.toml + the directory's .pad.toml override the same
// way getConfiguredConfig does, but treats "not configured" as a plain
// returned error the caller can retry on instead of exiting. EnsureServer
// (local-mode auto-start of the background padd process — the same
// thing every other `pad` command does transparently on first use) is
// still called, but its failure is likewise a plain error, not an exit.
//
// The ONLY genuinely unrecoverable state left is config.Load() failing
// (e.g. ~/.pad is unwritable/permission-denied) — that's a broken-
// machine condition no retry will fix, and it is the ONE case this
// function still surfaces as an actual Go error up to the loop below,
// which folds it into the same silent backoff-and-retry as every other
// failure rather than exiting even then: an unattended monitor should
// never terminate itself over a condition an operator might fix without
// restarting the whole Claude Code session.
func monitorClient() (*cli.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	applyPadTomlOverride(cfg)
	if !cfg.IsConfigured() {
		return nil, fmt.Errorf("pad is not configured")
	}
	if err := cli.EnsureServer(cfg); err != nil {
		return nil, fmt.Errorf("ensure server: %w", err)
	}
	return cli.NewClientFromURL(cfg.BaseURL()), nil
}

// runWatchMonitor is `pad watch --stream --for-session`'s main loop.
// Implements DOC-2479's silent-start contract exactly:
//   - no .pad.toml anywhere in the directory tree → sleep an hour,
//     retry, print NOTHING (exiting would end the monitor process the
//     plugin auto-started).
//   - client unconfigured, local server won't start, or padd otherwise
//     unreachable (dial failure, non-200, or the stream drops mid-read)
//     → backoff-retry, print NOTHING. See monitorClient's doc comment
//     for exactly which conditions this folds into "unreachable" rather
//     than treating as fatal.
//   - a matching event → exactly one stdout line, formatMonitorLine.
//
// Runs until ctx is cancelled (Ctrl+C / SIGTERM) or, on the initial
// call from Cobra's cmd.Context(), forever. The .pad.toml check and
// client construction both run INSIDE the loop, every iteration —
// deliberately not hoisted above it — so a workspace linked or a padd
// brought up mid-session is picked up on the next retry without a
// process restart.
// monitorSessionIdentity describes THIS monitor process to the server's
// presence registry (PLAN-2558 S2, TASK-2560): the working directory's
// basename as a human-readable label, plus this process's own pid.
//
// WHY NOT READ ~/.pad/sessions/, given that this slice exists to give
// `pad session register` a consumer. Because the monitor cannot tell
// WHICH registry entry is its own session without guessing. The entries
// are written by whatever process ran `pad session register` — an agent
// harness, a different pid — and the only fields available to match on
// are pid and cwd. Two agent sessions in the same checkout produce two
// entries with identical cwd, and picking "the newest" would be a
// coin flip that puts a wrong-but-confident name in the S5 target
// picker. Process ancestry would settle it exactly, and is
// platform-specific (this binary ships for macOS and Windows too), so
// it is not a slice-S2 shape.
//
// The monitor's own cwd + pid are never wrong, and they answer the
// question the label exists to answer: which project is this session
// working in. The registry's remaining value — correlating a stream to
// the agent session that spawned it — needs an identifier the harness
// passes down, not a heuristic; that is worth doing when something
// actually needs it, and worth NOT faking until then.
//
// Failures are silent by construction: os.Getwd can fail (a deleted
// cwd), and the answer is an unlabelled session, not a dead monitor.
// This whole file's contract is "print nothing, keep running" (see
// runWatchMonitor's doc comment).
func monitorSessionIdentity() cli.StreamSessionIdentity {
	ident := cli.StreamSessionIdentity{PID: os.Getpid()}
	if cwd, err := os.Getwd(); err == nil {
		// filepath.Base("/") is "/" and Base("") is "." — neither names
		// anything, so drop them rather than labelling a session "/".
		if base := filepath.Base(cwd); base != "" && base != "." && base != string(filepath.Separator) {
			ident.Label = base
		}
	}
	// PLAN-2613 S2: announce consent. A session arms when EITHER it has a
	// live local arm declaration (`pad session arm`, the explicit path) OR
	// its repository opted in via .pad.toml [push] auto_arm with no
	// per-user veto (the auto path, D4). Both default to off, so a monitor
	// on a repo that did neither stays unarmed — the safe skew during the
	// S2→S3 rollout, before the plugin's on-skill-invoke arming exists.
	// The server's own armed bit (D3/S1) remains the delivery authority;
	// this only decides what the client ANNOUNCES.
	//
	// This value is snapshotted here, at connect. A `pad session disarm`
	// that lands AFTER this read but before the stream request is sent
	// leaves the just-opened connection announcing armed=true until its
	// next reconnect (Codex R2 finding 4) — an inherent check-then-connect
	// TOCTOU, bounded to one reconnect window and re-evaluated every
	// reconnect (the monitor re-reads on each loop iteration). Fully
	// closing it needs a server-side disarm signal on an already-open
	// connection, which is S3's reconnect/heal job, not this snapshot's.
	ident.Armed = cli.SessionArmedLocally() || cli.ResolveAutoArmFromDisk().Armed
	return ident
}

func runWatchMonitor(ctx context.Context) error {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lastEventID string
	attempt := 0

	for {
		if sigCtx.Err() != nil {
			return nil
		}

		pt, _ := cli.LoadPadToml()
		if pt == nil {
			// Silent: no workspace linked anywhere above cwd. A
			// directory can gain a .pad.toml later (pad workspace init
			// run mid-session), so keep polling rather than exiting.
			if !sleepOrDone(sigCtx, noPadTomlRetryInterval) {
				return nil
			}
			continue
		}

		client, err := monitorClient()
		if err != nil {
			attempt++
			if !sleepOrDone(sigCtx, padddBackoff(attempt)) {
				return nil
			}
			continue
		}

		req, err := client.NewWatchEventsStreamRequest(sigCtx, lastEventID, monitorSessionIdentity())
		if err != nil {
			attempt++
			if !sleepOrDone(sigCtx, padddBackoff(attempt)) {
				return nil
			}
			continue
		}

		// No timeout: an SSE connection is meant to stay open for the
		// life of the monitor process. Deliberately a fresh client, not
		// client.streamClient (1h cap) or the Client's default 10s
		// client — see NewWatchEventsStreamRequest's doc comment.
		httpClient := &http.Client{}
		resp, err := httpClient.Do(req)
		if err != nil {
			if sigCtx.Err() != nil {
				return nil
			}
			attempt++
			if !sleepOrDone(sigCtx, padddBackoff(attempt)) {
				return nil
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			attempt++
			if !sleepOrDone(sigCtx, padddBackoff(attempt)) {
				return nil
			}
			continue
		}

		attempt = 0 // connected — reset backoff for the NEXT disconnect
		lastEventID = streamWatchEvents(resp, lastEventID)
		resp.Body.Close()

		if sigCtx.Err() != nil {
			return nil
		}
		// The stream ended (server closed it, network blip, padd
		// restart). Last-Event-ID (captured in streamWatchEvents)
		// resumes from here — no re-delivery, no gap silently
		// swallowed beyond the replay buffer's own bounds.
		attempt++
		if !sleepOrDone(sigCtx, padddBackoff(attempt)) {
			return nil
		}
	}
}

// streamWatchEvents reads SSE lines from an open connection, printing
// formatMonitorLine for each "notification" event, until the body is
// exhausted (connection closed) or a read error occurs. Returns the
// last-seen event ID for Last-Event-ID resume on reconnect.
//
// On "sync_required" (the server's signal that the requested
// Last-Event-ID has been evicted from its replay buffer — TASK-2533
// codex round 1 finding 6), this CLEARS the returned cursor rather than
// leaving it at the stale value. Without this, the caller's next
// reconnect would resend the SAME stale Last-Event-ID, the server would
// respond sync_required again, forever — an unrecoverable resume loop
// that only a process restart could break. Clearing it makes the next
// reconnect a fresh (non-resuming) subscription instead, which is
// exactly what sync_required is telling the caller to do: the gap is
// too large to replay, stop trying.
func streamWatchEvents(resp *http.Response, lastEventID string) string {
	scanner := bufio.NewScanner(resp.Body)
	var eventType, data string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			lastEventID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			switch eventType {
			case "notification":
				if data != "" {
					var payload watchStreamPayload
					if err := json.Unmarshal([]byte(data), &payload); err == nil {
						fmt.Println(formatMonitorLine(payload))
					}
				}
			case "sync_required":
				lastEventID = ""
			}
			eventType, data = "", ""
		}
	}
	return lastEventID
}
