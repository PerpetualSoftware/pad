package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/config"
)

var (
	version       = "dev"
	commit        = ""
	buildTime     = ""
	workspaceFlag string
	formatFlag    string
	urlFlag       string
)

// truthyEnv reports whether an environment-variable value means "on".
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func fullVersion() string {
	if commit == "" {
		return version
	}
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	if buildTime != "" {
		return version + " (" + short + " " + buildTime + ")"
	}
	return version + " (" + short + ")"
}

func main() {
	// Spec §8 fallback form: --cmdhelp-capabilities. Handled before
	// cobra parsing so the discovery bit is truly side-effect-free
	// (no flag-parse errors, no config load, no server reach-out, no
	// auth challenge) per spec requirements.
	if handleCmdhelpCapabilitiesFallback(os.Args[1:], os.Stdout) {
		return
	}

	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// handleCmdhelpCapabilitiesFallback scans args for the --cmdhelp-capabilities
// flag (cmdhelp v0.1 §8 fallback form). If found, it writes the
// capability bit to w and returns true so callers can short-circuit.
// Returns false (no output) when the flag isn't present.
//
// Extracted from main() so the side-effect-free guarantee can be
// asserted in tests without spawning a subprocess.
func handleCmdhelpCapabilitiesFallback(args []string, w io.Writer) bool {
	for _, a := range args {
		if a == "--cmdhelp-capabilities" {
			fmt.Fprintln(w, cmdhelp.CapabilityLine(padCmdhelpFormats))
			return true
		}
	}
	return false
}

// newRootCmd constructs the pad CLI's root cobra command tree.
//
// Extracted from main() so tests can use the real tree (golden
// snapshots, schema validation, example-vs-flag-tree drift detection)
// without running it. Build it, inspect it, never call Execute() in
// tests.
//
// Each call returns a fresh tree. The persistent flags bind to the
// package-level vars (workspaceFlag, formatFlag, urlFlag) — fine for
// tests as long as they don't run concurrently with the real CLI flow.
func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     "pad",
		Short:   "Pad — project management for developers and AI agents",
		Version: fullVersion(),
		// Runtime errors (item not found, network failures) print a clean
		// one-line message; the full usage block is noise there. Flag-parse
		// errors DO want usage, so the FlagErrorFunc below reprints it for
		// that path only.
		SilenceUsage: true,
		// Validate the persistent --format value up front so a bogus value
		// fails loudly instead of silently rendering a table. Runs for every
		// subcommand (cobra invokes the nearest PersistentPreRunE, and no
		// subcommand defines its own). The help command binds its own local
		// --format, which shadows this persistent flag, so `pad help … --format md`
		// leaves formatFlag at its "table" default and still passes.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			switch formatFlag {
			case "table", "json", "markdown":
				return nil
			default:
				return fmt.Errorf("invalid --format %q: must be one of table, json, markdown", formatFlag)
			}
		},
	}

	// SilenceUsage (above) suppresses usage for flag-parse errors too, but
	// those genuinely benefit from it. Reprint usage on the flag-error path
	// only — a typo'd flag stays helpful while runtime errors stay terse.
	// We print only the usage block here and let cobra print the "Error: …"
	// line (SilenceErrors stays off), so the message isn't duplicated.
	rootCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		cmd.PrintErrln(cmd.UsageString())
		return err
	})

	rootCmd.PersistentFlags().StringVar(&workspaceFlag, "workspace", "", "workspace slug override")
	rootCmd.PersistentFlags().StringVar(&formatFlag, "format", "table", "output format: table, json (markdown on select commands, e.g. item show, project changelog)")
	rootCmd.PersistentFlags().StringVar(&urlFlag, "url", "", "server URL override (e.g., https://app.getpad.dev)")

	rootCmd.AddCommand(
		padInitCmd(),
		authCmd(),
		serverCmd(),
		workspaceCmd(),
		projectCmd(),
		itemCmd(),
		collectionCmd(),
		libraryGroupCmd(),
		agentCmd(),
		githubCmd(),
		roleCmd(),
		tagCmd(),
		webhooksCmd(),
		attachmentCmd(),
		dbCmd(),
		completionCmd(),
		mcpCmd(),
		bootstrapCmd(),
		playbookCmd(),
	)

	rootCmd.SetHelpCommand(helpCmd())

	rootCmd.RegisterFlagCompletionFunc("workspace", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := config.Load()
		if err != nil || !cfg.IsConfigured() {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		client := cli.NewClientFromURL(cfg.BaseURL())
		workspaces, err := client.ListWorkspaces()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var slugs []string
		for _, ws := range workspaces {
			slugs = append(slugs, ws.Slug)
		}
		return slugs, cobra.ShellCompDirectiveNoFileComp
	})

	return rootCmd
}

// --- helpers ---

func getConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	// --url flag takes highest precedence. An explicit --url is an
	// unambiguous "talk to this server" signal from the user, so it
	// promotes Mode to remote even if the existing config.toml has
	// mode=local — without that promotion the .pad.toml URL pin
	// (padTomlURLFor) would treat the directory as local and skip
	// writing the URL. See BUG-1535.
	if urlFlag != "" {
		cfg.URL = urlFlag
		cfg.LoadedFromFlags = true
		if cfg.Mode == "" || cfg.Mode == config.ModeLocal {
			cfg.Mode = config.ModeRemote
		}
	}
	return cfg
}

// applyPadTomlOverride layers the per-directory .pad.toml `url` field on top
// of cfg when present and Mode is not already explicitly set by --url. The
// override exists so that a user in a directory linked to a non-local
// workspace (remote/cloud) talks to the correct server without needing
// --url on every command — see BUG-1535.
//
// IMPORTANT: this MUST NOT be applied to server/admin commands that
// configure or operate the local Pad server process — `pad server start`,
// `pad server stop`, `pad auth setup`, `pad auth configure`. For those
// commands the .pad.toml URL is a CLIENT-direction signal that would
// either contaminate the server's PublicLinkBaseURL or cause `pad auth
// setup` to refuse to run locally. Call this from client-API entry
// points (getConfiguredConfig, the pad init client phase) instead.
func applyPadTomlOverride(cfg *config.Config) {
	if cfg == nil {
		return
	}
	// An explicit --url flag (LoadedFromFlags) already represents the
	// user's intent; don't second-guess it with a directory pin.
	if cfg.LoadedFromFlags {
		return
	}
	pt, _ := cli.LoadPadToml()
	if pt == nil || pt.URL == "" {
		return
	}
	cfg.URL = pt.URL
	if cfg.Mode == "" || cfg.Mode == config.ModeLocal {
		cfg.Mode = config.ModeRemote
	}
	// Treat this as configured for IsConfigured() so client-API entry
	// points don't nag with a "not configured" prompt when the directory
	// is clearly linked to a specific server.
	cfg.LoadedFromFile = true
}

func getClient() (*cli.Client, *config.Config) {
	cfg := getConfiguredConfig()
	if err := cli.EnsureServer(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return cli.NewClientFromURL(cfg.BaseURL()), cfg
}

func getWorkspace() string {
	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return ws
}

func outputJSON(v interface{}) {
	cli.PrintJSON(v)
}

// --- serve ---

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for your shell.

Usage:
  source <(pad completion bash)
  pad completion zsh > "${fpath[1]}/_pad"
  pad completion fish | source`,
		Example: `  pad completion bash
  pad completion zsh
  pad completion fish
  pad completion powershell`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
}

// =============================================================================
// v2 Commands: create, list, show, update, delete, search, status, next, collections
// =============================================================================
