package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-3415 (lead ruling): the e2e harness's app-CA door exists only in a
// build with the e2etest tag. A default build, which is what release and
// production ship, must contain no source that reads PAD_E2E_APP_CA, so no
// environment variable can widen what a shipped binary trusts.
//
// It lists every Go file the pad binary compiles under the given tags
// (go list -deps) and reports which of them mention the variable. The
// e2etest list is the positive control: if the door moved or was renamed,
// the default-build check would pass vacuously.
func TestE2EAppCADoorIsNotInDefaultBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	const needle = "PAD_E2E_APP_CA"
	mentions := func(t *testing.T, tags string) []string {
		t.Helper()
		args := []string{"list", "-deps", "-json=Dir,GoFiles,Standard"}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, ".")
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go %s: %v", strings.Join(args, " "), err)
		}
		var found []string
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for dec.More() {
			var p struct {
				Dir      string
				GoFiles  []string
				Standard bool
			}
			if err := dec.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Standard {
				continue
			}
			for _, f := range p.GoFiles {
				b, err := os.ReadFile(filepath.Join(p.Dir, f))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), needle) {
					found = append(found, filepath.Join(filepath.Base(p.Dir), f))
				}
			}
		}
		return found
	}
	if got := mentions(t, "e2etest"); len(got) == 0 {
		t.Fatalf("control: no file in the e2etest build mentions %s; the door moved, so the default-build check below would prove nothing", needle)
	}
	if got := mentions(t, ""); len(got) != 0 {
		t.Fatalf("the default build compiles %v, which mention %s: the e2e CA door must exist only under -tags e2etest", got, needle)
	}
}
