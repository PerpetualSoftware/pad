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
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
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
	_ = http.ListenAndServe(net.JoinHostPort(host, port), mux)
}
