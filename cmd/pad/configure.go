package main

import (
	"bufio"
	"fmt"
	neturl "net/url"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/xarmian/pad/internal/config"
	"golang.org/x/term"
)

type configureValues struct {
	Mode string
	URL  string
	Host string
	Port int
}

func configureCmd() *cobra.Command {
	var values configureValues

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Configure how this Pad client connects to a server",
		Long: `Configure how this Pad client connects to a server.

Modes:
  cloud   This client connects to your Pad Cloud account at ` + config.CloudBaseURL + `.
          Managed, OAuth sign-in, no setup. Recommended.
  local   This client manages a local Pad server on this machine.
  remote  This client connects to your own self-hosted Pad server by base URL.`,
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			// Mirror pad init: an explicit cancel keyword from a prompt
			// surfaces as errCancelled. Convert to the canonical exit so
			// it doesn't render as a generic cobra error.
			defer func() {
				if isCancellation(retErr) {
					cancelInit()
				}
			}()

			cfg := getConfig()
			if err := runConfigureFlow(cfg, values); err != nil {
				return err
			}

			fmt.Printf("Configured Pad client mode: %s\n", cfg.Mode)
			fmt.Printf("  Server URL: %s\n", cfg.BaseURL())
			if cfg.Mode == config.ModeLocal {
				fmt.Printf("  Server bind: %s\n", cfg.Addr())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&values.Mode, "mode", "", "connection mode: cloud, local, remote")
	cmd.Flags().StringVar(&values.URL, "url", "", "server base URL for remote mode")
	cmd.Flags().StringVar(&values.Host, "host", "", "local server host override for local mode")
	cmd.Flags().IntVar(&values.Port, "port", 0, "local server port override for local mode")

	return cmd
}

func getConfiguredConfig() *config.Config {
	cfg := getConfig()
	if cfg.IsConfigured() {
		return cfg
	}

	if !canPromptForConfig() {
		fmt.Fprintln(os.Stderr, "Pad is not configured. Run 'pad auth configure' first.")
		os.Exit(1)
	}

	fmt.Println("Pad is not configured yet.")
	fmt.Println("Let's configure how this client connects to Pad.")
	fmt.Println()
	if err := runConfigureFlow(cfg, configureValues{}); err != nil {
		// User-initiated cancellation routes through the canonical
		// "Cancelled." + 130 exit even when triggered from this
		// non-init entry point.
		if isCancellation(err) {
			cancelInit()
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	cfg = getConfig()
	if !cfg.IsConfigured() {
		fmt.Fprintln(os.Stderr, "Error: Pad configuration was not saved. Run 'pad auth configure' again.")
		os.Exit(1)
	}
	return cfg
}

func runConfigureFlow(cfg *config.Config, values configureValues) error {
	if values.Mode == "" {
		if !canPromptForConfig() {
			return fmt.Errorf("missing --mode and no interactive terminal is available")
		}

		mode, err := promptForMode()
		if err != nil {
			return err
		}
		values.Mode = mode
	}

	if err := applyConfigureValues(cfg, &values); err != nil {
		return err
	}

	return cfg.Save()
}

func applyConfigureValues(cfg *config.Config, values *configureValues) error {
	mode := strings.ToLower(strings.TrimSpace(values.Mode))
	if !config.ValidMode(mode) {
		return fmt.Errorf("invalid mode %q", values.Mode)
	}

	switch mode {
	case config.ModeLocal:
		cfg.Mode = config.ModeLocal
		cfg.URL = ""
		if values.Host != "" {
			cfg.Host = strings.TrimSpace(values.Host)
		}
		if values.Port > 0 {
			cfg.Port = values.Port
		}
		if cfg.Host == "" {
			cfg.Host = "127.0.0.1"
		}
		if cfg.Port == 0 {
			cfg.Port = 7777
		}
		return nil
	case config.ModeRemote:
		if values.URL == "" {
			if !canPromptForConfig() {
				return fmt.Errorf("--url is required for %s mode", mode)
			}
			url, err := promptForValue("Server URL", "")
			if err != nil {
				return err
			}
			values.URL = url
		}
		normalizedURL, err := normalizeBaseURL(values.URL)
		if err != nil {
			return err
		}
		cfg.Mode = config.ModeRemote
		cfg.URL = normalizedURL
		return nil
	case config.ModeCloud:
		// Cloud is a labeled-Remote at runtime: same flow, hardcoded URL.
		// The CLI doesn't need to know about OAuth-vs-password — that
		// distinction lives entirely on the web side at /auth/cli/{code}.
		// Ignore any --url passed in; Cloud is anchored to the canonical
		// public endpoint.
		cfg.Mode = config.ModeCloud
		cfg.URL = config.CloudBaseURL
		return nil
	default:
		return fmt.Errorf("invalid mode %q", values.Mode)
	}
}

// modeOption describes one row in the connection-mode picker. Order in
// promptForMode dictates display order — Cloud is intentionally first as
// the recommended path for new users.
type modeOption struct {
	key         string   // canonical config.Mode* value
	icon        string   // emoji shown next to the label
	label       string   // short human label ("Cloud", "Local", "Remote")
	tagline     string   // primary one-line description
	subline     string   // optional second descriptive line ("" to omit)
	recommended bool     // adds a "(recommended)" marker after the label
	aliases     []string // case-insensitive accepted typed inputs (besides the row number)
}

func promptForMode() (string, error) {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)
	cyan := color.New(color.FgCyan)

	options := []modeOption{
		{
			key:         config.ModeCloud,
			icon:        "☁️ ",
			label:       "Cloud",
			tagline:     "Connect to your Pad Cloud account at " + config.CloudBaseURL,
			subline:     "Managed, OAuth sign-in, no setup",
			recommended: true,
			aliases:     []string{"cloud"},
		},
		{
			key:     config.ModeLocal,
			icon:    "💻",
			label:   "Local",
			tagline: "Run a Pad server on this machine (data stays on your computer)",
			aliases: []string{"local"},
		},
		{
			key:     config.ModeRemote,
			icon:    "🌐",
			label:   "Remote",
			tagline: "Connect to your own self-hosted Pad server by URL",
			aliases: []string{"remote"},
		},
	}

	fmt.Println()
	fmt.Println(bold.Sprint("How should this client connect to Pad?"))
	fmt.Println()
	for i, opt := range options {
		marker := ""
		if opt.recommended {
			marker = " " + cyan.Sprint("(recommended)")
		}
		fmt.Printf("  %d. %s  %s%s\n", i+1, opt.icon, bold.Sprint(opt.label), marker)
		fmt.Printf("        %s\n", opt.tagline)
		if opt.subline != "" {
			fmt.Printf("        %s\n", dim.Sprint(opt.subline))
		}
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Select a mode [1-%d, 'c' to cancel]: ", len(options))
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		input := strings.ToLower(strings.TrimSpace(line))

		switch input {
		case "c", "q", "cancel", "quit":
			return "", errCancelled
		}

		// Numeric selection (1-based to match the displayed indexes).
		if n := atoiSafe(input); n >= 1 && n <= len(options) {
			return options[n-1].key, nil
		}

		// Alias selection — accept "cloud", "local", "remote" as typed input.
		if matched := matchModeAlias(options, input); matched != "" {
			return matched, nil
		}

		fmt.Printf("Invalid choice %q. Enter a number between 1 and %d, a mode name, or 'c' to cancel.\n", input, len(options))
	}
}

// atoiSafe returns the decimal value of s, or 0 if s is not a positive
// decimal integer. Used by promptForMode where 0 (no match) is always an
// invalid mode selection so we don't need to distinguish "not a number"
// from "zero".
func atoiSafe(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}

func matchModeAlias(options []modeOption, input string) string {
	if input == "" {
		return ""
	}
	for _, opt := range options {
		for _, a := range opt.aliases {
			if a == input {
				return opt.key
			}
		}
	}
	return ""
}

func promptForValue(label, defaultValue string) (string, error) {
	reader := bufio.NewReader(os.Stdin)

	if defaultValue != "" {
		fmt.Printf("%s [%s, 'c' to cancel]: ", label, defaultValue)
	} else {
		fmt.Printf("%s ['c' to cancel]: ", label)
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	value := strings.TrimSpace(line)
	switch strings.ToLower(value) {
	case "c", "q", "cancel", "quit":
		// Recognized cancel keyword — let the caller convert to the
		// canonical 'pad init' exit. Important: when a defaultValue
		// is in scope, pressing enter still selects the default;
		// only an explicit keyword cancels.
		return "", errCancelled
	}
	if value == "" {
		value = defaultValue
	}
	return value, nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("server URL is required")
	}

	u, err := neturl.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL must use http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server URL must not include query params or fragments")
	}

	return strings.TrimRight(u.String(), "/"), nil
}

func canPromptForConfig() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
