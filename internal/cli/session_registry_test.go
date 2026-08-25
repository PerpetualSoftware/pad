package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// registryEnv gives a test a fresh HOME (so ~/.pad/sessions is empty) and
// a clean session environment, returning the sessions dir.
func registryEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearSessionEnv(t)
	return filepath.Join(home, ".pad", "sessions")
}

func numericFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if registryFileName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func readRegistration(t *testing.T, path string) (SessionRegistration, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var reg SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return reg, raw
}

// TestRegisterSession_KeyedOnHarnessPid is the counterfactual against the
// v1 registry: the file is named after the HARNESS session pid (CLAUDE_PID
// here — the parent of this test process, a live pid that is not our own),
// not after os.Getpid(). Under v1 this assertion fails with a file named
// after the test process.
func TestRegisterSession_KeyedOnHarnessPid(t *testing.T) {
	dir := registryEnv(t)
	parent := os.Getppid()
	t.Setenv("CLAUDE_PID", itoa(parent))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-abc")

	rec, err := RegisterSession("/some/project", "wren")
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	wantPath := filepath.Join(dir, fmt.Sprintf("%d.json", parent))
	if rec.Path != wantPath {
		t.Fatalf("record path = %q, want %q (keyed on CLAUDE_PID, not os.Getpid())", rec.Path, wantPath)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))); !os.IsNotExist(err) {
		t.Fatalf("a file keyed on the registrar's own pid must not exist (v1 keying); stat err = %v", err)
	}

	reg, raw := readRegistration(t, rec.Path)
	if reg.PID != parent || reg.PIDSource != "CLAUDE_PID" {
		t.Fatalf("owner = %d/%q, want %d/CLAUDE_PID", reg.PID, reg.PIDSource, parent)
	}
	if reg.RegistrarPID != os.Getpid() {
		t.Fatalf("registrar pid = %d, want %d (the v1 'pid' key keeps naming the writer)", reg.RegistrarPID, os.Getpid())
	}
	if raw["pid"] != float64(os.Getpid()) {
		t.Fatalf("the v1 JSON key 'pid' must survive for compatibility, raw = %v", raw)
	}
	if reg.Agent != "wren" || reg.Cwd != "/some/project" || reg.SessionID != "sess-abc" || reg.RegisteredAt == "" {
		t.Fatalf("record fields not round-tripped: %+v", reg)
	}
	if rec.SessionPID != parent || rec.Agent != "wren" || rec.Liveness != LivenessAlive || rec.Legacy {
		t.Fatalf("returned record wrong: %+v", rec)
	}
}

func TestRegisterSession_PadSessionPidOverridesClaudePid(t *testing.T) {
	dir := registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getpid()))
	t.Setenv("PAD_SESSION_PID", itoa(os.Getppid()))
	t.Setenv("PAD_SESSION_ID", "pad-id")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "claude-id")
	rec, err := RegisterSession("/p", "x")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rec.Path) != fmt.Sprintf("%d.json", os.Getppid()) {
		t.Fatalf("PAD_SESSION_PID must win: %s", rec.Path)
	}
	if rec.SessionPIDSource != "PAD_SESSION_PID" || rec.SessionID != "pad-id" {
		t.Fatalf("override source/id not recorded: %+v", rec)
	}
	if files := numericFiles(t, dir); len(files) != 1 {
		t.Fatalf("expected exactly one record, got %v", files)
	}
}

// TestRegisterSession_RepeatedCallsOneFile is the day-57 shape as a
// counterfactual: ten registrations from one session produce ONE record,
// the newest. (v1 produced ten, keyed on ten dead command pids.)
func TestRegisterSession_RepeatedCallsOneFile(t *testing.T) {
	dir := registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))
	var last SessionRecord
	for i := 0; i < 10; i++ {
		rec, err := RegisterSession(fmt.Sprintf("/dir/%d", i), "rook")
		if err != nil {
			t.Fatal(err)
		}
		last = rec
	}
	files := numericFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("ten registrations of one session must leave ONE file, got %v", files)
	}
	reg, _ := readRegistration(t, last.Path)
	if reg.Cwd != "/dir/9" {
		t.Fatalf("re-register must overwrite (refresh): cwd = %q", reg.Cwd)
	}
}

func TestRegisterSession_FilePermissions(t *testing.T) {
	registryEnv(t)
	rec, err := RegisterSession("/some/dir", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected mode 0600, got %o", info.Mode().Perm())
	}
}

func TestRegisterSession_AnonymousOmitsAgentKey(t *testing.T) {
	registryEnv(t)
	rec, err := RegisterSession("/some/dir", "")
	if err != nil {
		t.Fatal(err)
	}
	_, raw := readRegistration(t, rec.Path)
	if _, ok := raw["agent"]; ok {
		t.Fatalf("anonymous registration must omit the agent key, raw = %v", raw)
	}
	if _, ok := raw["messaging_socket_path"]; ok {
		t.Fatalf("no socket → no messaging_socket_path key, raw = %v", raw)
	}
}

func TestRegisterSession_PrunesDeadRecords(t *testing.T) {
	dir := registryEnv(t)
	dead := exitedProcessPID(t)
	writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: dead, PIDSource: "CLAUDE_PID"}, Agent: "ghost", Cwd: "/x", RegisteredAt: "2026-08-01T00:00:00Z"})
	deadPath := filepath.Join(dir, fmt.Sprintf("%d.json", dead))

	rec, err := RegisterSession("/y", "me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("register must prune the dead record; stat err = %v", err)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		t.Fatalf("own record must survive the prune: %v", err)
	}
}

func writeV2Record(t *testing.T, dir string, reg SessionRegistration) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", reg.PID))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeV1Record writes a registry file exactly as the pre-TASK-2767 code
// did: keyed on the registrar pid, no session_pid, no agent, a bare
// socket path.
func writeV1Record(t *testing.T, dir string, registrarPID int, socket string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"pid": registrarPID, "cwd": "/legacy", "registered_at": "2026-08-20T00:00:00Z"}
	if socket != "" {
		raw["messaging_socket_path"] = socket
	}
	data, _ := json.Marshal(raw)
	path := filepath.Join(dir, fmt.Sprintf("%d.json", registrarPID))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestListSessions_LegacyFiles: a v1 file is listed as legacy, owned by
// the pid in its socket basename (a LIVE harness pid here, so the row is
// alive) — never judged by the socket path itself, which recorded no
// identity. Without a parseable socket, the registrar pid is the owner,
// and a dead one reads dead.
func TestListSessions_LegacyFiles(t *testing.T) {
	dir := registryEnv(t)
	deadA, deadB := exitedProcessPID(t), exitedProcessPID(t)
	if deadA == deadB {
		t.Skip("two throwaway processes got the same pid; cannot build two distinct legacy files")
	}
	// Socket path names a live pid (ours) but does NOT exist on disk — the
	// socket must not be the liveness signal for a legacy row.
	writeV1Record(t, dir, deadA, filepath.Join(t.TempDir(), fmt.Sprintf("%d.sock", os.Getpid())))
	writeV1Record(t, dir, deadB, "")

	records, err := ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SessionRecord{}
	for _, r := range records {
		byPath[filepath.Base(r.Path)] = r
	}
	a := byPath[fmt.Sprintf("%d.json", deadA)]
	if !a.Legacy || a.SessionPID != os.Getpid() || a.SessionPIDSource != "legacy-socket" || a.Liveness != LivenessAlive || a.Agent != "" {
		t.Fatalf("legacy row with socket-derived owner wrong: %+v", a)
	}
	b := byPath[fmt.Sprintf("%d.json", deadB)]
	if !b.Legacy || b.SessionPID != deadB || b.SessionPIDSource != "legacy-registrar" || b.Liveness != LivenessDead {
		t.Fatalf("legacy row with registrar owner wrong: %+v", b)
	}
}

func TestListSessions_SkipsArmFilesListsMalformed(t *testing.T) {
	dir := registryEnv(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Neighbours in the same directory that are NOT sessions.
	for _, name := range []string{"arm-sess-abcdef.json", ".arm-123.tmp", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"armed":true,"pid":1}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bad := filepath.Join(dir, "4242.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * time.Hour)
	if err := os.Chtimes(bad, old, old); err != nil {
		t.Fatal(err)
	}

	records, err := ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected only the malformed numeric file to list, got %+v", records)
	}
	r := records[0]
	if !r.Malformed || r.Liveness != LivenessUnknown || r.Path != bad {
		t.Fatalf("malformed record wrong: %+v", r)
	}
	if ts, err := time.Parse(time.RFC3339, r.RegisteredAt); err != nil || time.Since(ts) < 199*time.Hour {
		t.Fatalf("malformed record must carry the file mtime as its timestamp, got %q (%v)", r.RegisteredAt, err)
	}
}

func TestListSessions_NewestFirst(t *testing.T) {
	dir := registryEnv(t)
	self := os.Getpid()
	parent := os.Getppid()
	writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: self, PIDSource: "self"}, Agent: "older", Cwd: "/a", RegisteredAt: "2026-08-01T00:00:00Z"})
	writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: parent, PIDSource: "CLAUDE_PID"}, Agent: "newer", Cwd: "/b", RegisteredAt: "2026-08-02T00:00:00Z"})
	records, err := ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Agent != "newer" || records[1].Agent != "older" {
		t.Fatalf("expected newest first, got %+v", records)
	}
}

func TestPruneSessions(t *testing.T) {
	dir := registryEnv(t)
	dead := exitedProcessPID(t)
	alivePath := writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: os.Getpid(), PIDSource: "self"}, Agent: "live", Cwd: "/a", RegisteredAt: "2026-08-01T00:00:00Z"})
	deadPath := writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: dead, PIDSource: "CLAUDE_PID"}, Agent: "gone", Cwd: "/b", RegisteredAt: "2026-08-01T00:00:00Z"})
	unknownPath := filepath.Join(dir, "777777.json")
	if err := os.WriteFile(unknownPath, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(unknownPath, old, old); err != nil {
		t.Fatal(err)
	}

	// No age bound: dead goes, alive and unknown stay.
	rep, err := PruneSessions(0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.DeadRemoved != 1 || rep.UnknownRemoved != 0 || rep.Kept != 2 || len(rep.Removed) != 1 || rep.Removed[0].Path != deadPath {
		t.Fatalf("prune(0) report wrong: %+v", rep)
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("dead record must be removed; stat err = %v", err)
	}
	for _, p := range []string{alivePath, unknownPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive prune(0): %v", p, err)
		}
	}

	// An age bound that the unknown record is YOUNGER than: still kept.
	rep, err = PruneSessions(200*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.UnknownRemoved != 0 || rep.Kept != 2 {
		t.Fatalf("young unknown must be kept under a 200h bound: %+v", rep)
	}
	// An age bound it is older than: removed. Alive is never touched.
	rep, err = PruneSessions(50*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.UnknownRemoved != 1 || rep.DeadRemoved != 0 || rep.Kept != 1 {
		t.Fatalf("old unknown must be removed under a 50h bound: %+v", rep)
	}
	if _, err := os.Stat(unknownPath); !os.IsNotExist(err) {
		t.Fatalf("old unknown record must be removed; stat err = %v", err)
	}
	if _, err := os.Stat(alivePath); err != nil {
		t.Fatalf("alive record must never be pruned: %v", err)
	}
}

// TestSessionRecord_JSONShape pins the consumer contract of `pad session
// list --format json`: the keys scripts read, present in the shape they
// expect, with agent always present (never omitted when anonymous).
func TestSessionRecord_JSONShape(t *testing.T) {
	registryEnv(t)
	rec, err := RegisterSession("/proj", "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_pid", "session_pid_source", "agent", "cwd", "liveness", "registered_at", "path"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("record JSON missing %q: %s", key, data)
		}
	}
	if raw["agent"] != "" || raw["liveness"] != "alive" || raw["session_pid"] != float64(os.Getpid()) {
		t.Fatalf("record JSON values wrong: %s", data)
	}
	for _, absent := range []string{"legacy", "malformed", "session_id"} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("%q must be omitted when false/empty: %s", absent, data)
		}
	}
}

// TestListSessions_SemanticallyMalformedIsUnknown: JSON that parses but
// is not a registration this code wrote must be MALFORMED (unknown, kept
// without an age bound) — not a dead legacy record to prune, and not a
// live one (codex round 1 P2).
func TestListSessions_SemanticallyMalformedIsUnknown(t *testing.T) {
	dir := registryEnv(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"1001.json": `{}`,
		"1002.json": `{"session_pid":1}`,
		"1003.json": `{"session_pid":1,"registered_at":"not-a-time"}`,
		"1004.json": `{"session_pid":-5,"registered_at":"2026-08-01T00:00:00Z"}`,
		"1005.json": `{"pid":0,"registered_at":"2026-08-01T00:00:00Z"}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	records, err := ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(cases) {
		t.Fatalf("expected %d records, got %+v", len(cases), records)
	}
	for _, r := range records {
		if !r.Malformed || r.Liveness != LivenessUnknown {
			t.Fatalf("%s must be malformed+unknown: %+v", filepath.Base(r.Path), r)
		}
	}
	rep, err := PruneSessions(0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 || rep.Kept != len(cases) {
		t.Fatalf("an unbounded prune must keep every malformed file: %+v", rep)
	}
}

// TestRegistry_LockFileIsNotASession: the mutation lock lives in the
// registry directory and must never list as a session.
func TestRegistry_LockFileIsNotASession(t *testing.T) {
	dir := registryEnv(t)
	if _, err := RegisterSession("/p", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionsLockName)); err != nil {
		t.Fatalf("register must take the directory lock (lock file missing): %v", err)
	}
	records, err := ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("only the registration may list, got %+v", records)
	}
}

// TestRegistry_ConcurrentRegisterAndPrune: many registers and prunes at
// once, in-process, end with exactly the live record and no error. This
// exercises the lock + re-read path under contention; it cannot prove the
// cross-process race is closed (that is flock's contract, stated in
// session_lock_unix.go), only that the mutators do not corrupt each other.
func TestRegistry_ConcurrentRegisterAndPrune(t *testing.T) {
	dir := registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := RegisterSession(fmt.Sprintf("/d/%d", i), "a"); err != nil {
				errs <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, err := PruneSessions(0, time.Now()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent mutation error: %v", err)
	}
	files := numericFiles(t, dir)
	if len(files) != 1 || files[0] != fmt.Sprintf("%d.json", os.Getppid()) {
		t.Fatalf("expected exactly the live record, got %v", files)
	}
}
