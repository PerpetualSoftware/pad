package main

import (
	"bufio"
	"fmt"
	neturl "net/url"
	"os"
	"strings"

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
  local   This client manages a local Pad server.
  remote  This client connects to another Pad server by base URL.
  docker  This client connects to a Docker-managed Pad server, usually at localhost.

Pad Cloud mode is reserved for a future release and is not yet available.`,
		RunE: func(cmd *cobra.Command, args []string) error {
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

	cmd.Flags().StringVar(&values.Mode, "mode", "", "connection mode: local, remote, docker")
	cmd.Flags().StringVar(&values.URL, "url", "", "server base URL for remote or docker mode")
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
		fmt.Fprintln(os.Stderr, "Pad is not configured. Run 'pad configure' first.")
		os.Exit(1)
	}

	fmt.Println("Pad is not configured yet.")
	fmt.Println("Let's configure how this client connects to Pad.")
	fmt.Println()
	if err := runConfigureFlow(cfg, configureValues{}); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	cfg = getConfig()
	if !cfg.IsConfigured() {
		fmt.Fprintln(os.Stderr, "Error: Pad configuration was not saved. Run 'pad configure' again.")
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
	case config.ModeRemote, config.ModeDocker:
		if values.URL == "" {
			if !canPromptForConfig() {
				return fmt.Errorf("--url is required for %s mode", mode)
			}
			prompt := "Server URL"
			defaultURL := "http://127.0.0.1:7777"
			if mode == config.ModeRemote {
				defaultURL = ""
			}
			url, err := promptForValue(prompt, defaultURL)
			if err != nil {
				return err
			}
			values.URL = url
		}
		normalizedURL, err := normalizeBaseURL(values.URL)
		if err != nil {
			return err
		}
		cfg.Mode = mode
		cfg.URL = normalizedURL
		return nil
	case config.ModeCloud:
		return fmt.Errorf("Pad Cloud is not available yet")
	default:
		return fmt.Errorf("invalid mode %q", values.Mode)
	}
}

func promptForMode() (string, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("How should this client connect to Pad?")
	fmt.Println("  1. Local")
	fmt.Println("  2. Remote")
	fmt.Println("  3. Docker")
	fmt.Println()

	for {
		fmt.Print("Select a mode [1-3]: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}

		switch strings.TrimSpace(line) {
		case "1", "local", "Local":
			return config.ModeLocal, nil
		case "2", "remote", "Remote":
			return config.ModeRemote, nil
		case "3", "docker", "Docker":
			return config.ModeDocker, nil
		default:
			fmt.Println("Enter 1, 2, or 3.")
		}
	}
}

func promptForValue(label, defaultValue string) (string, error) {
	reader := bufio.NewReader(os.Stdin)

	if defaultValue != "" {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	} else {
		fmt.Printf("%s: ", label)
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	value := strings.TrimSpace(line)
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
