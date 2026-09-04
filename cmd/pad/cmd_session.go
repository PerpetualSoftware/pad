package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

// sessionCmd groups the local session-registry verbs (register / list /
// prune — TASK-2533, TASK-2767) and the arm/disarm/status consent verbs
// (PLAN-2613 S2, TASK-2617).
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage this session's pad registry entry and push-arming",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		sessionRegisterCmd(),
		sessionListCmd(),
		sessionPruneCmd(),
		sessionArmCmd(),
		sessionDisarmCmd(),
		sessionStatusCmd(),
		sessionShouldArmCmd(),
		sessionFirstConnectCmd(),
	)
	return cmd
}

// sessionFirstConnectCmd is `pad session first-connect` (PLAN-2613 S3,
// D8): reports whether this session's first-connect boot ritual should run
// now and marks it done, so /pad:connect fires the workspace's
// on-session-start playbooks exactly once per session. Prints
// {"first_connect": bool}. Not hidden from JSON callers but off the main
// help — it is a plumbing verb the connect skill calls, paired with arm.
func sessionFirstConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "first-connect",
		Short:  "Report+mark whether this is the session's first connect (internal; used by /pad:connect)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			first, err := cli.MarkFirstConnect()
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{"first_connect": first})
			}
			if first {
				fmt.Println("first-connect")
			} else {
				fmt.Println("reconnect")
			}
			return nil
		},
	}
}

// sessionShouldArmCmd is `pad session should-arm` (PLAN-2613 S3): a quiet
// exit-code gate for the plugin monitor wrapper. Exit 0 means this session
// should announce armed right now (a live explicit arm, or auto_arm with
// no explicit disarm); exit 1 means it should not. It prints nothing, so
// the wrapper can branch on `$?` without parsing. Hidden — it is an
// internal plugin contract, not a user verb (the user surface is
// arm/disarm/status).
//
// This is what makes the monitor-existence gate real (D1): the `always`
// auto-arm monitor runs this once and exits when it returns non-zero, so
// no consent → no stream, and the per-reconnect re-check lets a
// within-session disarm stop the stream on its next reconnect.
func sessionShouldArmCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "should-arm",
		Short:         "Exit 0 if this session should announce armed (internal; used by the plugin monitor)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cli.ResolveAnnouncedArmed() {
				return nil // exit 0 — should arm
			}
			// Non-zero exit, no output. The sentinel message is silenced.
			return errNotArmed
		},
	}
}

// errNotArmed is the silent sentinel for `pad session should-arm`'s
// exit-1 path — SilenceErrors keeps it off the terminal.
var errNotArmed = fmt.Errorf("not armed")

// sessionRegisterCmd records the CURRENT session in the local session
// registry (~/.pad/sessions/<session_pid>.json — see
// internal/cli/session_registry.go). The plugin monitor runs it on start;
// other harnesses call it from their own session-start hook.
func sessionRegisterCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this session, and the agent it runs as, in the local session registry",
		Long: `Records this session — the harness session process (from $PAD_SESSION_PID,
else $CLAUDE_PID, else this process), its working directory, the agent
name it runs as, and its messaging socket's identity — to
~/.pad/sessions/<session_pid>.json, mode 0600. 'pad session list' reads
the registry back with a liveness verdict per session.

The agent name defaults to what every write is attributed to (.pad.toml
agent_name, else $PAD_AGENT, else the detected runtime); --agent overrides
it, and --agent "" registers an anonymous session. A registered name is
then what this session's writes carry from that point on — the record is
consulted before .pad.toml and the environment (BUG-2882) — so re-registering
under a different name re-attributes the session's later writes with it. An
anonymous registration leaves an environment-declared name in force.

Safe to call from any directory — it does not require a linked workspace
(.pad.toml) — and safe to call more than once per session: each call
overwrites the session's record with a fresh timestamp. Records of
sessions it can see are dead are pruned on the way (see 'pad session
prune').`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			cwd = canonicalDir(cwd)
			// An explicit --agent wins verbatim, including the empty string
			// (anonymous). Absent, the session is named the way its writes
			// are.
			if !cmd.Flags().Changed("agent") {
				agent = cli.ResolveAgentName()
			}
			rec, err := cli.RegisterSession(cwd, agent)
			if err != nil {
				return fmt.Errorf("register session: %w", err)
			}
			if formatFlag == "json" {
				return cli.PrintJSON(rec)
			}
			name := rec.Agent
			if name == "" {
				name = "(anonymous)"
			}
			fmt.Printf("Registered session pid %d as %s at %s\n", rec.SessionPID, printableCell(name), rec.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent name to register as; becomes the name this session's writes carry (default: the current one; \"\" for an anonymous row)")
	return cmd
}

// sessionListCmd is `pad session list` (TASK-2767): the registry read-back,
// one row per registered session with a liveness verdict. Deterministic
// and local: it stats sockets and probes pids, it does not ask the server.
// By default shows sessions that are alive or unknown (a platform that
// cannot probe); --all adds the dead ones a prune would remove.
func sessionListCmd() *cobra.Command {
	var (
		agentFilter string
		cwdFilter   string
		all         bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered sessions on this machine with a liveness verdict each",
		Long: `Reads ~/.pad/sessions and reports each registered session: its owner pid,
the agent it registered as, its working directory, and whether it is
alive right now.

Liveness is decided locally and fails toward honesty: "alive" means every
signal the record carries — its messaging socket, its owner pid, and the
identity captured for each at registration where the platform supplies
one — still checks out; "dead" means one is gone or has been reused by
something else; "unknown" means one could not be examined (a platform
that cannot probe pids, a socket that cannot be stat'ed). A legacy row
(registered before sessions carried an agent name) is judged by its pid
alone: it can say a session exists but not who it is, and it matches no
--agent filter.

Everything a row says about WHO — agent, session id, and (unless
session_pid_verified is true) the owner pid itself — is what the session
declared. On Linux the pid claim is checked against the registering
process's ancestry; a consumer that needs that check reads
session_pid_verified in the JSON.

Deciding whether a NAME is in use from this output (the rule a script
needs, and where it is only as good as its inputs):

  - List WITHOUT --agent (use --cwd to scope), then apply the rule
    yourself: a row with liveness "alive", no legacy/malformed flag, and
    session_pid_verified true, whose agent is the name, means IN USE by
    that session (identify it by session_id, else session_pid).
  - Any "unknown", legacy, or malformed row scoped to the same directory
    is INDETERMINATE, never "not in use" — such a row cannot say who it
    is, so --agent NAME would have hidden it.
  - No matching row means "no registered row", not "nobody": a harness
    that never registers, or whose registration failed, is invisible.
  - Two or more alive rows for one name is ambiguous; do not pick the
    newest — registered_at is each session's own clock.
  - The registry is this OS user's; other users' sessions are not here.
  - The verdict is a sample: a session can register or exit right after.
  - A row carries the name at registration; a window that changes its
    PAD_AGENT afterwards must run 'pad session register' again.

Newest first. Use --format json for the stable shape.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			records, err := cli.ListSessions()
			if err != nil {
				return err
			}
			var filtered []cli.SessionRecord
			cwdWant := ""
			if cwdFilter != "" {
				cwdWant = canonicalDir(cwdFilter)
			}
			for _, r := range records {
				if !all && r.Liveness == cli.LivenessDead {
					continue
				}
				// A legacy or malformed row has NO agent — not an empty one —
				// so it matches no --agent filter, including --agent "".
				if cmd.Flags().Changed("agent") && (r.Agent != agentFilter || r.Legacy || r.Malformed) {
					continue
				}
				// Directory IDENTITY, not spelling: both sides resolve
				// symlinks so a checkout reached through a link matches its
				// real path (codex round 5). A stored cwd that no longer
				// exists compares by its cleaned spelling.
				if cwdWant != "" && canonicalDir(r.Cwd) != cwdWant {
					continue
				}
				filtered = append(filtered, r)
			}
			if formatFlag == "json" {
				if filtered == nil {
					filtered = []cli.SessionRecord{}
				}
				return cli.PrintJSON(filtered)
			}
			printSessionList(filtered)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentFilter, "agent", "", "only sessions registered as this agent name (\"\" matches anonymous sessions)")
	cmd.Flags().StringVar(&cwdFilter, "cwd", "", "only sessions registered from this directory (symlinks resolved on both sides)")
	cmd.Flags().BoolVar(&all, "all", false, "include dead sessions")
	return cmd
}

func printSessionList(records []cli.SessionRecord) {
	if len(records) == 0 {
		fmt.Println("No registered sessions.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tPID\tSTATE\tCWD\tREGISTERED")
	for _, r := range records {
		agent := r.Agent
		if agent == "" {
			agent = "-"
		}
		state := string(r.Liveness)
		switch {
		case r.Malformed:
			state += " (malformed)"
		case r.Legacy:
			state += " (legacy)"
		case !r.SessionPIDVerified:
			state += " (unverified pid)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", printableCell(agent), r.SessionPID, state, printableCell(r.Cwd), printableCell(r.RegisteredAt))
	}
	tw.Flush()
}

// canonicalDir resolves a directory to its real, absolute, cleaned path
// when it exists (symlinks followed), and to its cleaned absolute
// spelling otherwise — so registration and --cwd compare directory
// identity rather than the string a shell happened to use.
func canonicalDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

// printableCell makes a self-declared value safe for the terminal table:
// every non-printable rune (newline, carriage return, ESC, ...) becomes
// '?', so a hostile agent name or directory cannot forge a row or drive
// the terminal (codex round 3). Replaced, not dropped, so the value is
// visibly odd rather than silently shortened. JSON output is untouched —
// encoding/json escapes on its own.
func printableCell(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// sessionPruneCmd is `pad session prune` (TASK-2767): removes the records
// of dead sessions. Alive records are never touched. Unknown ones — a
// platform that cannot probe pids, or a malformed file — are removed only
// under an explicit --older-than bound, because "cannot tell" is not
// "dead" and deleting on it would empty a live registry on such a platform.
func sessionPruneCmd() *cobra.Command {
	var olderThan time.Duration
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove dead sessions' records from the local session registry",
		Long: `Deletes registry records whose session is dead. Never deletes a record
it can see is alive.

Records whose liveness is unknown (this platform cannot probe the owner,
or the file is malformed) are kept unless --older-than is given, in which
case those older than the bound are deleted too — live or not, since
unknown means exactly that; it is the only way such a registry ever
shrinks, and an explicit choice for that reason.

'pad session register' already prunes the records it can see are dead on
every call; this verb is for the unknown ones and for cleaning up
without registering.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := cli.PruneSessions(olderThan, time.Now())
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				if rep.Removed == nil {
					rep.Removed = []cli.SessionRecord{}
				}
				return cli.PrintJSON(rep)
			}
			fmt.Printf("Pruned %d dead", rep.DeadRemoved)
			if olderThan > 0 {
				fmt.Printf(" and %d unknown older than %s", rep.UnknownRemoved, olderThan)
			}
			fmt.Printf("; kept %d.\n", rep.Kept)
			return nil
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "also remove unknown-liveness records older than this (e.g. 72h)")
	return cmd
}

// sessionArmCmd is `pad session arm` (PLAN-2613 S2, TASK-2617): the
// explicit in-session consent verb. It declares that THIS session accepts
// `pad push` notifications by writing the local arm-state file
// (internal/cli/session_arm_state.go) that a monitor reads to decide
// whether the stream it opens announces armed=true.
//
// Idempotent (D7): re-running it just refreshes the file. The effect
// reaches the server the next time this session's stream connects, since
// arming is declared at connect (the server bit from S1 is the delivery
// authority, D3); a monitor already streaming before the arm re-reads on
// its next reconnect — mid-session re-arm of a live monitor is the S3
// lockfile's job, not this verb's.
func sessionArmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "arm",
		Short: "Declare that this session accepts pad push notifications",
		Long: `Arm this session: declare consent to receive 'pad push' notifications.

Writes a local, session-scoped arm-state file. A push monitor for this
session announces armed=true to the server on its next stream connect,
and only armed sessions receive pushes (the consent gate). Run
'pad session disarm' to withdraw consent, or 'pad session status' to see
the current state.

Arming is per-session and transient. To opt a whole REPOSITORY in without
arming each session by hand, set 'push.auto_arm = true' under a [push]
table in the repo's .pad.toml (a deliberate, committed choice).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cli.WriteArmState()
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"armed": true,
					"path":  path,
				})
			}
			fmt.Println("Armed this session — it will accept pad push notifications.")
			fmt.Println("Takes effect when the session's push monitor next connects.")
			fmt.Println("Run 'pad session disarm' to withdraw, or 'pad session status' to check.")
			return nil
		},
	}
}

// sessionDisarmCmd is `pad session disarm` (PLAN-2613 S3, TASK-2618): the
// withdrawal verb. It writes a session-scoped explicit-OFF marker (NOT a
// file removal), so the session stops accepting pushes for its remaining
// life EVEN in an auto_arm=true repo — the disconnect verb must not be a
// lie there. The marker dies with the session (same liveness as an arm),
// so across sessions auto_arm remains the standing contract; permanent-off
// is a .pad.toml edit. Idempotent.
func sessionDisarmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disarm",
		Short: "Withdraw this session's consent to pad push notifications",
		Long: `Disarm this session: stop accepting 'pad push' notifications for the
rest of this session.

This holds even when the repository opted in via .pad.toml
'push.auto_arm = true' — the disconnect is session-scoped and wins for this
session. It does NOT revoke the repo's standing consent: a NEW session in
an auto_arm repo arms again. To turn auto-arm off permanently, remove
'push.auto_arm' from .pad.toml (the same deliberate edit that turned it on).

Idempotent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := cli.WriteDisarmState()
			if err != nil {
				return err
			}
			// Whether auto_arm would otherwise re-arm THIS session tells the
			// user if the disarm actually changed anything visible.
			autoArm := cli.ResolveAutoArmFromDisk().Armed
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"armed":              false,
					"disarmed":           true,
					"path":               path,
					"auto_arm_would_arm": autoArm,
				})
			}
			fmt.Println("Disarmed this session — it no longer accepts pad push notifications.")
			if autoArm {
				fmt.Println("(This repo has push.auto_arm=true; a NEW session will arm again. Remove it from .pad.toml to stop that.)")
			}
			return nil
		},
	}
}

// sessionStatusJSON is the JSON shape of `pad session status` (a stable
// contract S4's composer and other tooling can read).
type sessionStatusJSON struct {
	Workspace string `json:"workspace,omitempty"`
	// LocalState is THIS session's tri-state local override (PLAN-2613 S3):
	// "armed" (explicit `pad session arm`), "disarmed" (explicit `pad
	// session disarm`, which beats auto_arm for this session), or "none"
	// (no live override — auto_arm decides). It reflects intent on this
	// machine, not server state.
	LocalState string `json:"local_state"`
	// LocalArmed is the boolean shorthand: LocalState == "armed". Retained
	// for a simple read; LocalState carries the full tri-state.
	LocalArmed bool `json:"local_armed"`
	// AutoArm is the resolved .pad.toml + per-user config decision (the
	// auto path).
	AutoArm     bool `json:"auto_arm"`
	AutoArmRepo bool `json:"auto_arm_repo"`
	AutoArmVeto bool `json:"auto_arm_user_veto"`
	// AutoArmConfigError is true when the per-user config.toml exists but
	// couldn't be read; auto-arm then resolves off (fail closed) and this
	// says so, so the off state reads as "couldn't confirm" rather than
	// "not opted in".
	AutoArmConfigError bool `json:"auto_arm_config_error"`
	// Announced is what a stream this session opens now would declare
	// (ResolveAnnouncedArmed): a live explicit local override wins (armed
	// arms, disarmed does not), otherwise auto_arm decides.
	Announced bool `json:"announced_armed"`
	// ServerReachable reports whether the server answered; when false the
	// session counts are unknown, not zero.
	ServerReachable bool `json:"server_reachable"`
	Connected       int  `json:"connected_sessions"`
	Accepting       int  `json:"accepting_sessions"`
}

// sessionStatusCmd is `pad session status` (PLAN-2613 S2, TASK-2617):
// reports this session's arming — the resolved local/auto decision and
// what a stream would announce — plus the server's own view of how many
// of the user's sessions are connected and accepting pushes.
//
// Deliberately resilient: an unreachable server degrades the session
// counts to "unknown" rather than failing the command, because the local
// arm/config half is still worth reporting when padd is down.
func sessionStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show this session's push-arming state and the server's session view",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			decision := cli.ResolveAutoArmFromDisk()
			localState := cli.SessionArmState()

			st := sessionStatusJSON{
				LocalState:         localArmStateString(localState),
				LocalArmed:         localState == cli.LocalArmOn,
				AutoArm:            decision.Armed,
				AutoArmRepo:        decision.RepoAutoArm,
				AutoArmVeto:        decision.UserVeto,
				AutoArmConfigError: decision.ConfigUnreadable,
				Announced:          cli.ResolveAnnouncedArmed(),
			}
			if ws, err := cli.DetectWorkspace(workspaceFlag); err == nil {
				st.Workspace = ws
			}

			// Server view — best effort. Build the client without
			// getClient()'s os.Exit-on-failure so a down server degrades
			// gracefully (see this command's doc comment).
			if counts, ok := serverSessionCounts(); ok {
				st.ServerReachable = true
				st.Connected = counts.connected
				st.Accepting = counts.accepting
			}

			if formatFlag == "json" {
				return cli.PrintJSON(st)
			}
			printSessionStatus(st)
			return nil
		},
	}
}

type sessionCounts struct{ connected, accepting int }

// serverSessionCounts fetches the caller's live sessions from the server,
// returning ok=false (not an error) when the server can't be reached or
// the client isn't configured — the caller reports "unknown", never a
// misleading zero.
func serverSessionCounts() (sessionCounts, bool) {
	cfg, err := config.Load()
	if err != nil {
		return sessionCounts{}, false
	}
	// Resolve the target server exactly as the client entry points do: an
	// explicit --url wins (and promotes Mode to remote), otherwise the
	// directory's .pad.toml `url` override applies. Without this, status
	// would query the global config's server while the monitor connects to
	// the repo's (Codex R1 MED-2), and `--url B` would be ignored entirely
	// (Codex R2 finding 5).
	if urlFlag != "" {
		cfg.URL = urlFlag
		cfg.LoadedFromFlags = true
		if cfg.Mode == "" || cfg.Mode == config.ModeLocal {
			cfg.Mode = config.ModeRemote
		}
	}
	applyPadTomlOverride(cfg) // no-op when --url set (LoadedFromFlags)
	if !cfg.IsConfigured() {
		return sessionCounts{}, false
	}
	if err := cli.EnsureServer(cfg); err != nil {
		return sessionCounts{}, false
	}
	client := cli.NewClientFromURL(cfg.BaseURL())
	resp, err := client.ListSessions()
	if err != nil || resp == nil {
		return sessionCounts{}, false
	}
	var counts sessionCounts
	counts.connected = resp.Count
	for _, s := range resp.Sessions {
		if s.Armed {
			counts.accepting++
		}
	}
	return counts, true
}

// localArmStateString maps the tri-state to the stable JSON token.
func localArmStateString(s cli.LocalArmState) string {
	switch s {
	case cli.LocalArmOn:
		return "armed"
	case cli.LocalArmOff:
		return "disarmed"
	case cli.LocalArmError:
		return "error"
	default:
		return "none"
	}
}

func printSessionStatus(st sessionStatusJSON) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	ws := st.Workspace
	if ws == "" {
		ws = "(not linked)"
	}
	fmt.Fprintf(tw, "Workspace:\t%s\n", ws)

	local := "none (no explicit arm/disarm this session)"
	switch st.LocalState {
	case "armed":
		local = "armed (pad session arm)"
	case "disarmed":
		local = "disarmed (pad session disarm) — wins for this session"
	case "error":
		local = "unreadable local state — failing closed (run pad session arm to reset)"
	}
	fmt.Fprintf(tw, "Local state:\t%s\n", local)

	auto := "off"
	switch {
	case st.AutoArm:
		auto = "on (.pad.toml push.auto_arm)"
	case st.AutoArmConfigError:
		auto = "off (user config unreadable — failing closed)"
	case st.AutoArmVeto:
		auto = "off (vetoed by per-user config)"
	}
	fmt.Fprintf(tw, "Auto-arm:\t%s\n", auto)

	announced := "no — this session will NOT receive pushes"
	if st.Announced {
		announced = "yes — this session declares consent on connect"
	}
	fmt.Fprintf(tw, "Announced:\t%s\n", announced)

	// The honest tri-state note the ruling requires: an explicit disarm in
	// an auto_arm repo holds only for this session.
	if st.LocalState == "disarmed" && st.AutoArm {
		fmt.Fprintf(tw, "Note:\tdisarmed this session; auto_arm will re-arm the next session\n")
	}

	if st.ServerReachable {
		fmt.Fprintf(tw, "Server sees:\t%d connected, %d accepting pushes\n", st.Connected, st.Accepting)
	} else {
		fmt.Fprintf(tw, "Server sees:\t(unreachable — session counts unknown)\n")
	}
	tw.Flush()
}
