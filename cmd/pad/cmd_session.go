package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

// sessionCmd groups session-registry commands (TASK-2533) and the
// arm/disarm/status consent verbs (PLAN-2613 S2, TASK-2617).
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage this session's pad registry entry and push-arming",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		sessionRegisterCmd(),
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

// sessionRegisterCmd registers the CURRENT process in the on-disk
// session registry (~/.pad/sessions/<pid>.json). See
// internal/cli/session_registry.go's package doc comment for what this
// is for and — as important — what it is NOT for yet: Phase 1's nudge
// delivery (the plugin monitor) needs no session registry at all; this
// is forward-looking infra for Phase 3's live-sessions/presence surface
// (IDEA-2464).
func sessionRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register",
		Short: "Register this session in the local pad session registry",
		Long: `Records this process's pid, working directory, and (when running inside a
Claude Code session) the CLAUDE_CODE_MESSAGING_SOCKET path to
~/.pad/sessions/<pid>.json.

Safe to call from any directory — it does not require a linked
workspace (.pad.toml) — and safe to call more than once per session
(each call overwrites this pid's record with a fresh timestamp).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			socketPath := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")

			path, err := cli.RegisterSession(cwd, socketPath)
			if err != nil {
				return fmt.Errorf("register session: %w", err)
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]string{
					"path": path,
					"cwd":  cwd,
				})
			}
			fmt.Printf("Registered session (pid %d) at %s\n", os.Getpid(), path)
			return nil
		},
	}
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
