// Command stub is a fake `pad` for scripts/install-refresh.sh's tests.
//
// It is a COMPILED BINARY on purpose. The script finds the running server
// with `pgrep -x <name>`, which matches /proc/<pid>/comm — and for a
// shebang script comm is the INTERPRETER's name ("bash"), not the script's.
// A shell stub therefore cannot be found the way the real, compiled `pad`
// is, and the first version of these tests failed for exactly that reason:
// the fixture, not the script.
//
// Behaviour is driven by env vars so the RESTART the script performs — which
// inherits the environment and not our arguments — behaves the same way.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	// STUB_STRAY_HOP is the middle process of the double fork in spawnStray:
	// it starts the stray and exits at once, so the stray's parent is NOT the
	// process that spawned it. The script accepts the restart's direct child
	// as the restart itself, so a stray that stayed a direct child would be
	// accepted and the test would measure nothing.
	if os.Getenv("STUB_STRAY_HOP") == "1" {
		cmd := exec.Command(os.Args[0], "server", "start")
		cmd.Env = strayEnv()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = cmd.Start()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		// The version can depend on WHERE this copy lives, which is what
		// makes the post-copy outcome check testable at all. The script has
		// two commit checks answering different questions — "is the source
		// right" before the kill, "is the destination right" after the copy
		// — and a fixture where both files report the same thing cannot
		// tell them apart: removing the second check left the suite green
		// until this existed.
		// Keyed on the EXACT PATH, not the directory. The script now stages
		// the copy as a temp file BESIDE the final path, so a
		// directory-keyed fixture would make the staged file report the
		// wrong version too and fire the earlier check — which is what
		// happened, and which would have left the destination check
		// untested again.
		if p := os.Getenv("STUB_INSTALLED_PATH"); p != "" && os.Args[0] == p {
			fmt.Println(os.Getenv("STUB_VERSION_INSTALLED"))
			return
		}
		fmt.Println(os.Getenv("STUB_VERSION"))
		return
	}

	if logPath := os.Getenv("STUB_ARGV_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
			f.Close()
		}
	}

	if !(len(os.Args) > 2 && os.Args[1] == "server" && os.Args[2] == "start") {
		return
	}
	// STUB_CWD_LOG records the directory each server start runs in, so a test
	// can see where a restart put it (BUG-3196).
	if logPath := os.Getenv("STUB_CWD_LOG"); logPath != "" {
		if wd, err := os.Getwd(); err == nil {
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintln(f, wd)
				f.Close()
			}
		}
	}
	// STUB_MARKER_LOG records, at each server start, the refresh marker at
	// STUB_MARKER_PATH as it reads right then, or "absent" (BUG-3215). The
	// restart is spawned inside the window the marker covers, so its line is
	// the marker as a CLI in that window would have found it.
	if logPath := os.Getenv("STUB_MARKER_LOG"); logPath != "" {
		line := "absent"
		if b, err := os.ReadFile(os.Getenv("STUB_MARKER_PATH")); err == nil {
			line = strings.TrimSpace(string(b))
		}
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
	// STUB_STRAY_ON_START=1: this start is beaten to the port by a stray
	// (BUG-3215, the post-probe check). The stray binds first; this process
	// then fails its own bind and exits, as the refresh's restart did on
	// day 79.
	if os.Getenv("STUB_STRAY_ON_START") == "1" {
		spawnStray()
	}
	// STUB_HEALTHY=0 exits immediately: what a restart that silently did
	// not happen looks like from outside.
	if os.Getenv("STUB_HEALTHY") != "1" {
		return
	}

	host := "127.0.0.1"
	for i, a := range os.Args {
		if a == "--host" && i+1 < len(os.Args) {
			host = os.Args[i+1]
		}
	}
	port := os.Getenv("STUB_PORT")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	// The stub's own pid, so a fixture can prove that the process answering
	// at an address is this one rather than whatever else holds the port
	// (TASK-3146). A real pad has no such route, and the script never asks.
	mux.HandleFunc("/stub/pid", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, os.Getpid())
	})
	// A port that cannot be bound exits the process, as ListenAndServe did.
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return
	}
	// STUB_ANNOUNCE=1 prints the port ACTUALLY bound, once listening, so a
	// test can start a server on port 0 and learn which port it got from the
	// process itself (BUG-3144). A test that picks a "free" port and then
	// dials it cannot tell its own stub from whatever else took that port in
	// between, and a stub that lost the race has already exited.
	if os.Getenv("STUB_ANNOUNCE") == "1" {
		fmt.Printf("LISTENING %d\n", ln.Addr().(*net.TCPAddr).Port)
	}
	// STUB_STRAY_ON_TERM=1: on SIGTERM, release the port and let a stray
	// take it before exiting (BUG-3215, the pre-spawn check). That is the
	// day-79 order: the refresh stops the server, and a CLI auto-start takes
	// the port before the refresh restarts it.
	if os.Getenv("STUB_STRAY_ON_TERM") == "1" {
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		go func() {
			<-term
			_ = ln.Close()
			spawnStray()
			os.Exit(0)
		}()
		// Closing the listener makes Serve return; main must not return
		// with it, or the process exits before the stray exists.
		_ = http.Serve(ln, mux)
		select {}
	}
	_ = http.Serve(ln, mux)
}

// strayEnv is this process's environment without the knobs that would make
// the stray do anything but serve: no stray of its own, no log lines that a
// test counts as the refresh's restart.
func strayEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "STUB_STRAY_ON_START", "STUB_STRAY_ON_TERM", "STUB_STRAY_HOP",
			"STUB_ARGV_LOG", "STUB_CWD_LOG", "STUB_MARKER_LOG", "STUB_ANNOUNCE", "STUB_HEALTHY":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "STUB_HEALTHY=1")
}

// spawnStray starts a detached server on STUB_PORT through a double fork and
// waits until it answers, so the caller's next step meets a port that is
// already taken. A stray that never answers fails the fixture loudly rather
// than letting the test pass on an empty port.
func spawnStray() {
	hop := exec.Command(os.Args[0])
	hop.Env = append(strayEnv(), "STUB_STRAY_HOP=1")
	if err := hop.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "stub: stray hop:", err)
		os.Exit(3)
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", os.Getenv("STUB_PORT")) + "/stub/pid"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for i := 0; i < 100; i++ {
		if res, err := client.Get(url); err == nil {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 64))
			res.Body.Close()
			if pid, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil && pid != os.Getpid() {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "stub: the stray never answered")
	os.Exit(3)
}
