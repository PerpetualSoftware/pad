package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/config"
)

type serverInfoReport struct {
	Config     serverInfoConfig     `json:"config"`
	Connection serverInfoConnection `json:"connection"`
	Auth       serverInfoAuth       `json:"auth"`
	Workspace  serverInfoWorkspace  `json:"workspace"`
	Local      *serverInfoLocal     `json:"local,omitempty"`
}

type serverInfoConfig struct {
	Configured         bool   `json:"configured"`
	Mode               string `json:"mode,omitempty"`
	ConfigPath         string `json:"config_path"`
	DataDir            string `json:"data_dir"`
	BaseURL            string `json:"base_url"`
	Host               string `json:"host,omitempty"`
	Port               int    `json:"port,omitempty"`
	ManagesLocalServer bool   `json:"manages_local_server"`
	LoadedFromFile     bool   `json:"loaded_from_file"`
	LoadedFromEnv      bool   `json:"loaded_from_env"`
	LoadedFromFlags    bool   `json:"loaded_from_flags"`
}

type serverInfoConnection struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

type serverInfoAuth struct {
	CredentialsPath        string         `json:"credentials_path"`
	CredentialsPresent     bool           `json:"credentials_present"`
	CredentialsServerURL   string         `json:"credentials_server_url,omitempty"`
	CredentialsMatchServer bool           `json:"credentials_match_server"`
	Authenticated          bool           `json:"authenticated"`
	SessionValid           bool           `json:"session_valid"`
	SetupRequired          bool           `json:"setup_required"`
	SetupMethod            string         `json:"setup_method,omitempty"`
	User                   *cli.LoginUser `json:"user,omitempty"`
}

type serverInfoWorkspace struct {
	Current string `json:"current,omitempty"`
}

type serverInfoLocal struct {
	BindAddr      string `json:"bind_addr"`
	ServerRunning bool   `json:"server_running"`
	PID           *int   `json:"pid,omitempty"`
	PIDFile       string `json:"pid_file"`
	LogFile       string `json:"log_file"`
	DBPath        string `json:"db_path"`
	DBExists      bool   `json:"db_exists"`
	DBSizeBytes   int64  `json:"db_size_bytes"`
}

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show local Pad client, connection, and server status",
		Long: `Show a client-oriented snapshot of this Pad installation and connection state.

This command reports local configuration, reachability, auth/session state, and
local runtime details when applicable. It does not auto-start the server and it
does not inspect remote server internals.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := collectServerInfo(getConfig())
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				outputJSON(report)
				return nil
			}

			printServerInfo(report)
			return nil
		},
	}
}

func collectServerInfo(cfg *config.Config) (*serverInfoReport, error) {
	report := &serverInfoReport{
		Config: serverInfoConfig{
			Configured:         cfg.IsConfigured(),
			Mode:               cfg.Mode,
			ConfigPath:         cfg.ConfigPath,
			DataDir:            cfg.DataDir,
			BaseURL:            cfg.BaseURL(),
			Host:               cfg.Host,
			Port:               cfg.Port,
			ManagesLocalServer: cfg.ManagesLocalServer(),
			LoadedFromFile:     cfg.LoadedFromFile,
			LoadedFromEnv:      cfg.LoadedFromEnv,
			LoadedFromFlags:    cfg.LoadedFromFlags,
		},
	}

	if ws, err := cli.DetectWorkspace(workspaceFlag); err == nil {
		report.Workspace.Current = ws
	}

	credsPath, err := cli.CredentialsPath()
	if err != nil {
		return nil, err
	}
	report.Auth.CredentialsPath = credsPath

	creds, err := cli.LoadCredentials()
	if err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	if creds != nil {
		report.Auth.CredentialsPresent = true
		report.Auth.CredentialsServerURL = creds.ServerURL
		report.Auth.CredentialsMatchServer = normalizeURL(creds.ServerURL) == normalizeURL(cfg.BaseURL())
	}

	client := cli.NewClientFromURL(cfg.BaseURL())
	client.SetAuthToken("")

	if err := client.Health(); err != nil {
		report.Connection.Error = err.Error()
	} else {
		report.Connection.Reachable = true
		if creds != nil && creds.Token != "" && report.Auth.CredentialsMatchServer {
			client.SetAuthToken(creds.Token)
		}
		session, err := client.CheckSession()
		if err != nil {
			report.Connection.Error = err.Error()
		} else {
			report.Auth.SetupRequired = session.SetupRequired
			report.Auth.SetupMethod = session.SetupMethod
			report.Auth.Authenticated = session.Authenticated
			report.Auth.SessionValid = session.Authenticated
			if session.Authenticated {
				user := session.User
				report.Auth.User = &user
			}
		}
	}

	if includeLocalRuntime(cfg) {
		report.Local = collectLocalInfo(cfg)
	}

	return report, nil
}

func includeLocalRuntime(cfg *config.Config) bool {
	if cfg.Mode == config.ModeLocal {
		return true
	}
	return !cfg.IsConfigured() && cfg.URL == ""
}

func collectLocalInfo(cfg *config.Config) *serverInfoLocal {
	info := &serverInfoLocal{
		BindAddr:      cfg.Addr(),
		ServerRunning: cli.IsServerRunning(cfg),
		PIDFile:       cfg.PIDFile(),
		LogFile:       cfg.LogFile(),
		DBPath:        cfg.DBPath,
	}

	if pid, ok := readPID(cfg.PIDFile()); ok {
		info.PID = &pid
	}

	if stat, err := os.Stat(cfg.DBPath); err == nil {
		info.DBExists = true
		info.DBSizeBytes = stat.Size()
	}

	return info
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func normalizeURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func printServerInfo(report *serverInfoReport) {
	mode := report.Config.Mode
	if mode == "" {
		mode = "not configured"
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Println("Client")
	fmt.Fprintf(w, "Configured:\t%s\n", yesNo(report.Config.Configured))
	fmt.Fprintf(w, "Mode:\t%s\n", mode)
	fmt.Fprintf(w, "Server URL:\t%s\n", report.Config.BaseURL)
	fmt.Fprintf(w, "Auto-manage local server:\t%s\n", yesNo(report.Config.ManagesLocalServer))
	fmt.Fprintf(w, "Config path:\t%s\n", report.Config.ConfigPath)
	fmt.Fprintf(w, "Data dir:\t%s\n", report.Config.DataDir)
	if report.Config.Host != "" {
		fmt.Fprintf(w, "Host:\t%s\n", report.Config.Host)
	}
	if report.Config.Port != 0 {
		fmt.Fprintf(w, "Port:\t%d\n", report.Config.Port)
	}
	fmt.Fprintf(w, "Config source:\t%s\n", configSource(report.Config))

	fmt.Println()
	fmt.Println("Connection")
	fmt.Fprintf(w, "Reachable:\t%s\n", yesNo(report.Connection.Reachable))
	if report.Connection.Error != "" {
		fmt.Fprintf(w, "Last error:\t%s\n", report.Connection.Error)
	}

	fmt.Println()
	fmt.Println("Auth")
	fmt.Fprintf(w, "Credentials file:\t%s\n", report.Auth.CredentialsPath)
	fmt.Fprintf(w, "Credentials present:\t%s\n", yesNo(report.Auth.CredentialsPresent))
	if report.Auth.CredentialsPresent {
		fmt.Fprintf(w, "Credentials server:\t%s\n", report.Auth.CredentialsServerURL)
		fmt.Fprintf(w, "Credentials match current server:\t%s\n", yesNo(report.Auth.CredentialsMatchServer))
	}
	fmt.Fprintf(w, "Session valid:\t%s\n", yesNo(report.Auth.SessionValid))
	fmt.Fprintf(w, "Authenticated:\t%s\n", yesNo(report.Auth.Authenticated))
	fmt.Fprintf(w, "Setup required:\t%s\n", yesNo(report.Auth.SetupRequired))
	if report.Auth.SetupMethod != "" {
		fmt.Fprintf(w, "Setup method:\t%s\n", report.Auth.SetupMethod)
	}
	if report.Auth.User != nil {
		fmt.Fprintf(w, "User:\t%s <%s>\n", report.Auth.User.Name, report.Auth.User.Email)
	}

	fmt.Println()
	fmt.Println("Workspace")
	currentWorkspace := report.Workspace.Current
	if currentWorkspace == "" {
		currentWorkspace = "(none linked)"
	}
	fmt.Fprintf(w, "Current workspace:\t%s\n", currentWorkspace)

	if report.Local != nil {
		fmt.Println()
		fmt.Println("Local Runtime")
		fmt.Fprintf(w, "Bind address:\t%s\n", report.Local.BindAddr)
		fmt.Fprintf(w, "Server running:\t%s\n", yesNo(report.Local.ServerRunning))
		if report.Local.PID != nil {
			fmt.Fprintf(w, "PID:\t%d\n", *report.Local.PID)
		}
		fmt.Fprintf(w, "PID file:\t%s\n", report.Local.PIDFile)
		fmt.Fprintf(w, "Log file:\t%s\n", report.Local.LogFile)
		fmt.Fprintf(w, "Database path:\t%s\n", report.Local.DBPath)
		fmt.Fprintf(w, "Database exists:\t%s\n", yesNo(report.Local.DBExists))
		if report.Local.DBExists {
			fmt.Fprintf(w, "Database size:\t%s\n", humanizeBytes(report.Local.DBSizeBytes))
		}
	}

	w.Flush()
}

func configSource(cfg serverInfoConfig) string {
	var sources []string
	if cfg.LoadedFromFile {
		sources = append(sources, "file")
	}
	if cfg.LoadedFromEnv {
		sources = append(sources, "env")
	}
	if cfg.LoadedFromFlags {
		sources = append(sources, "flags")
	}
	if len(sources) == 0 {
		return "defaults"
	}
	return strings.Join(sources, ", ")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func humanizeBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", value/1024)
}
