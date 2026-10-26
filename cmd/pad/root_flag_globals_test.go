package main

import "testing"

// preserveRootFlagGlobals restores workspaceFlag, formatFlag and urlFlag when
// the test ends. They are bound to the root command's persistent flags, so a
// test that parses --workspace, --format or --url in-process writes process
// state every later test reads; urlFlag outranks PAD_URL, so a leaked URL
// points the next test's CLI at a closed stub server (BUG-4086).
func preserveRootFlagGlobals(t testing.TB) {
	t.Helper()
	ws, format, url := workspaceFlag, formatFlag, urlFlag
	t.Cleanup(func() { workspaceFlag, formatFlag, urlFlag = ws, format, url })
}
