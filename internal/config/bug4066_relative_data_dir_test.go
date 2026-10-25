package config

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

// BUG-4066 phase 1 (lead ruling 4b4cb4ff): a relative PAD_DATA_DIR still
// resolves against this process's cwd, unchanged, but the process WARNS,
// naming the absolute directory it resolved, because a CLI and a server in
// different directories silently use different data.
func TestBUG4066_RelativeDataDirWarnsNamingTheAbsolutePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", "data")
	a := t.TempDir()
	resetRelativeDataDirWarning()

	var cfgA *Config
	t.Chdir(a)
	out := captureStderr(t, func() {
		var err error
		if cfgA, err = Load(); err != nil {
			t.Fatal(err)
		}
	})
	absA, _ := filepath.Abs("data")
	if cfgA.DataDir != "data" {
		t.Fatalf("resolution moved: DataDir = %q, want the verbatim %q (phase 1 changes nothing but the warning)", cfgA.DataDir, "data")
	}
	if !strings.Contains(out, "PAD_DATA_DIR is relative (data); this process resolves it to "+absA+".") {
		t.Fatalf("no warning naming %s; stderr = %q", absA, out)
	}

}

// The rest of the rule (4b4cb4ff): which variable is named, silence for an
// absolute data dir, once per process, and the same rule in userConfigPath.
func TestBUG4066_RelativeDataDirWarningRule(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	abs := func(v string) string { a, _ := filepath.Abs(v); return a }
	for _, tc := range []struct {
		name, dataDir, dbPath string
		want                  string // "" = silent
	}{
		{"relative PAD_DATA_DIR", "data", "", "PAD_DATA_DIR is relative (data); this process resolves it to " + abs("data") + "."},
		{"dot-relative PAD_DATA_DIR", "./data", "", "PAD_DATA_DIR is relative (./data); this process resolves it to " + abs("./data") + "."},
		{"relative PAD_DB_PATH", "", "pad.db", "PAD_DB_PATH is relative (pad.db); this process resolves the data dir to " + abs(".") + "."},
		{"relative PAD_DB_PATH beats an absolute PAD_DATA_DIR", cwd, "x/pad.db", "PAD_DB_PATH is relative (x/pad.db); this process resolves the data dir to " + abs("x") + "."},
		{"absolute PAD_DB_PATH makes a relative PAD_DATA_DIR moot", "data", filepath.Join(cwd, "pad.db"), ""},
		{"absolute PAD_DATA_DIR is silent", filepath.Join(cwd, "d"), "", ""},
		{"neither set is silent", "", "", ""},
		{"unexpanded tilde gets the hint", "~/pad", "", "The leading ~ was not expanded by your shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PAD_DATA_DIR", tc.dataDir)
			t.Setenv("PAD_DB_PATH", tc.dbPath)
			resetRelativeDataDirWarning()
			out := captureStderr(t, func() {
				if _, err := Load(); err != nil {
					t.Fatal(err)
				}
			})
			if tc.want == "" {
				if out != "" {
					t.Fatalf("want silence, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want %q in %q", tc.want, out)
			}
			if !strings.Contains(out, "relative values will be refused in a future release") {
				t.Fatalf("the warning does not say relative values will be refused: %q", out)
			}
		})
	}

	t.Run("once per process", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PAD_DATA_DIR", "data")
		t.Setenv("PAD_DB_PATH", "")
		resetRelativeDataDirWarning()
		out := captureStderr(t, func() {
			for i := 0; i < 3; i++ {
				if _, err := Load(); err != nil {
					t.Fatal(err)
				}
			}
			_ = userConfigPath()
		})
		if n := strings.Count(out, "is relative"); n != 1 {
			t.Fatalf("want exactly one warning per process, got %d: %q", n, out)
		}
	})

	t.Run("userConfigPath shares the rule", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PAD_DATA_DIR", "data")
		t.Setenv("PAD_DB_PATH", "")
		resetRelativeDataDirWarning()
		var path string
		out := captureStderr(t, func() { path = userConfigPath() })
		if path != filepath.Join("data", "config.toml") {
			t.Fatalf("userConfigPath = %q, want the verbatim data/config.toml (resolution unchanged)", path)
		}
		if !strings.Contains(out, "PAD_DATA_DIR is relative (data)") {
			t.Fatalf("userConfigPath read a relative data dir without the warning: %q", out)
		}
	})

	t.Run("a sink receives it instead of stderr", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PAD_DATA_DIR", "data")
		t.Setenv("PAD_DB_PATH", "")
		resetRelativeDataDirWarning()
		var got string
		SetRelativeDataDirWarningSink(func(m string) { got = m })
		t.Cleanup(func() { SetRelativeDataDirWarningSink(nil) })
		out := captureStderr(t, func() {
			if _, err := Load(); err != nil {
				t.Fatal(err)
			}
		})
		if out != "" || !strings.Contains(got, "PAD_DATA_DIR is relative (data)") {
			t.Fatalf("sink got %q, stderr got %q; want the line in the sink only", got, out)
		}
	})
}

// Windows: a drive-relative path ("C:data") and a rooted one with no drive
// ("\\data") are not absolute there, so they warn (4b4cb4ff, Q4).
func TestBUG4066_WindowsNonAbsolutePathsWarn(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}
	t.Chdir(t.TempDir())
	for _, v := range []string{`C:data`, `\data`} {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PAD_DATA_DIR", v)
		t.Setenv("PAD_DB_PATH", "")
		resetRelativeDataDirWarning()
		out := captureStderr(t, func() { _ = userConfigPath() })
		if !strings.Contains(out, "PAD_DATA_DIR is relative ("+v+")") {
			t.Fatalf("%q did not warn: %q", v, out)
		}
	}
}
