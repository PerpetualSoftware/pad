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
	)
	return cmd
}

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

// sessionDisarmCmd is `pad session disarm` (PLAN-2613 S2, TASK-2617): the
// withdrawal verb. It removes this session's arm-state file, so a monitor
// stops announcing armed on its next connect. Idempotent — disarming a
// session that was never armed is a success, not an error.
func sessionDisarmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disarm",
		Short: "Withdraw this session's consent to pad push notifications",
		Long: `Disarm this session: remove its local arm-state file so it stops
accepting 'pad push' notifications.

Idempotent — disarming a session that was not armed still succeeds.

Note: this withdraws an EXPLICIT 'pad session arm'. If the repository
opted in via .pad.toml 'push.auto_arm = true', the session still auto-arms
by policy; remove that setting to stop it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			removed, path, err := cli.RemoveArmState()
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"armed":     false,
					"was_armed": removed,
					"path":      path,
				})
			}
			if removed {
				fmt.Println("Disarmed this session — it no longer accepts pad push notifications.")
			} else {
				fmt.Println("This session was not armed; nothing to disarm.")
			}
			return nil
		},
	}
}

// sessionStatusJSON is the JSON shape of `pad session status` (a stable
// contract S4's composer and other tooling can read).
type sessionStatusJSON struct {
	Workspace string `json:"workspace,omitempty"`
	// LocalArmed is whether THIS session has a live local arm declaration
	// (the explicit `pad session arm` path). It reflects intent on this
	// machine, not server state.
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
	// Announced is what a stream this session opens now would declare:
	// LocalArmed OR AutoArm.
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
			localArmed := cli.SessionArmedLocally()

			st := sessionStatusJSON{
				LocalArmed:         localArmed,
				AutoArm:            decision.Armed,
				AutoArmRepo:        decision.RepoAutoArm,
				AutoArmVeto:        decision.UserVeto,
				AutoArmConfigError: decision.ConfigUnreadable,
				Announced:          localArmed || decision.Armed,
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
	// Apply the directory's .pad.toml `url` override exactly as the
	// monitor does (cmd_watch.go's monitorClient), so status queries the
	// SAME server the monitor connects to. Without this, a repo whose
	// .pad.toml points at server B would report server A's sessions — or,
	// with only a repo URL set, wrongly report the server unreachable
	// (Codex R1 MED-2). The override can also flip Mode to remote, which
	// makes an otherwise-"not configured" global config usable.
	applyPadTomlOverride(cfg)
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

func printSessionStatus(st sessionStatusJSON) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	ws := st.Workspace
	if ws == "" {
		ws = "(not linked)"
	}
	fmt.Fprintf(tw, "Workspace:\t%s\n", ws)

	local := "not armed"
	if st.LocalArmed {
		local = "armed (pad session arm)"
	}
	fmt.Fprintf(tw, "Local arm:\t%s\n", local)

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

	if st.ServerReachable {
		fmt.Fprintf(tw, "Server sees:\t%d connected, %d accepting pushes\n", st.Connected, st.Accepting)
	} else {
		fmt.Fprintf(tw, "Server sees:\t(unreachable — session counts unknown)\n")
	}
	tw.Flush()
}
