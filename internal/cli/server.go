package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/config"
)

// EnsureServer checks if the pad server is running; if not, starts it in the background.
func EnsureServer(cfg *config.Config) error {
	if isServerHealthy(cfg.Host, cfg.Port) {
		return nil
	}

	// Start server as background process
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.Command(exePath, "serve")
	setSysProcAttr(cmd)

	// Redirect stdout/stderr to log file
	logFile, err := os.OpenFile(cfg.LogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start server: %w", err)
	}

	// Write PID file
	if err := os.WriteFile(cfg.PIDFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
		logFile.Close()
		return fmt.Errorf("write PID file: %w", err)
	}

	// Release the process so it doesn't become a zombie
	cmd.Process.Release()
	logFile.Close()

	// Wait for server to become healthy
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if isServerHealthy(cfg.Host, cfg.Port) {
			return nil
		}
	}

	return fmt.Errorf("server failed to start within 3 seconds. Check %s for errors", cfg.LogFile())
}

// StopServer sends a stop signal to the background server process.
func StopServer(cfg *config.Config) error {
	pidData, err := os.ReadFile(cfg.PIDFile())
	if err != nil {
		return fmt.Errorf("server not running (no PID file)")
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("invalid PID file")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("process not found")
	}

	if err := stopProcess(process); err != nil {
		os.Remove(cfg.PIDFile())
		return fmt.Errorf("failed to stop server: %w", err)
	}

	os.Remove(cfg.PIDFile())

	// Wait for it to actually stop
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isServerHealthy(cfg.Host, cfg.Port) {
			return nil
		}
	}

	return nil
}

// IsServerRunning checks if the server is currently running.
func IsServerRunning(cfg *config.Config) bool {
	return isServerHealthy(cfg.Host, cfg.Port)
}

func isServerHealthy(host string, port int) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/api/v1/health", host, port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
