package cli

// On-disk session registry (TASK-2533; reworked in TASK-2767 for IDEA-2750
// part 2).
//
// ~/.pad/sessions/<session_pid>.json — one record per SESSION, keyed on
// the OWNER pid (the agent harness's session process, see SessionOwner),
// carrying the agent name the session declared. `pad session register`
// writes it; `pad session list` reads it with a liveness verdict per row;
// `pad session prune` removes the ones it can see are dead. The plugin monitor registers
// on start, so any Claude Code session with the plugin has a record from
// its first second; any other harness calls register from its own
// session-start hook with PAD_SESSION_PID / PAD_AGENT set.
//
// WHAT IT ANSWERS. "Which of my sessions on this machine are alive right
// now, and which agent is each one running as?" — a local, deterministic
// read that needs no server and no guessing. It is NOT the server's
// presence registry (`pad session status` / GET /api/v1/sessions): that
// surface exists only while an armed monitor holds a stream, refreshes on
// its own clock, and knows nothing about agent names. The two are kept
// separate on purpose — blending two sources with different failure
// modes would produce a list that is wrong in ways neither is alone.
//
// WHY THE KEY CHANGED. v1 keyed the file on os.Getpid() — the pid of the
// `pad session register` subprocess, which is dead before anyone reads
// the file. One session produced a new file per call and its own pid
// appeared in none of them; the only live identifier was the pid a
// reader could parse out of messaging_socket_path's basename. v2 keys on
// the harness-provided owner pid and records the socket's identity, so a
// re-register overwrites (a refresh) and liveness is a stat, not a parse.
// Legacy v1 files (no session_pid) still list — see legacyOwner — and
// prune like any other record.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionRegistration is one session's on-disk record.
type SessionRegistration struct {
	// SessionOwner supplies session_pid, session_pid_source, proc_start,
	// messaging_socket_path and the socket identity fields.
	SessionOwner
	// SessionID is the harness's own session identifier when it exports
	// one ($PAD_SESSION_ID, else $CLAUDE_CODE_SESSION_ID) — a handle for
	// tooling that addresses sessions by id (transcript paths, session
	// measurement). Opaque to pad.
	SessionID string `json:"session_id,omitempty"`
	// Agent is the name the session declared for itself — the same
	// self-declared value ResolveAgentName stamps on every write, recorded
	// so a reader can tell two sessions in one checkout apart by what they
	// are working AS. Empty is an anonymous session.
	Agent string `json:"agent,omitempty"`
	// RegistrarPID is the pid of the process that wrote the file (the
	// `pad` command). Its JSON key is v1's "pid", kept for compatibility;
	// it is informational — the OWNER is SessionOwner.PID.
	RegistrarPID int `json:"pid"`
	// Cwd is the directory the session was registered from.
	Cwd string `json:"cwd"`
	// RegisteredAt is RFC3339 UTC.
	RegisteredAt string `json:"registered_at"`
}

// SessionRecord is one registry entry as `pad session list` reports it:
// the registration plus the reader's verdict on it. Its JSON shape is the
// stable contract consumers script against.
type SessionRecord struct {
	// SessionPID is the owner pid — session_pid for a v2 record, derived
	// for a legacy one (see Legacy).
	SessionPID int `json:"session_pid"`
	// SessionPIDSource is where SessionPID came from: "PAD_SESSION_PID",
	// "CLAUDE_PID", "self", or for legacy files "legacy-socket" (parsed
	// from the socket basename) / "legacy-registrar" (the v1 pid field).
	SessionPIDSource string `json:"session_pid_source,omitempty"`
	// SessionPIDVerified is true when the recorded pid was checked against
	// the registering process's ancestry (Linux) or is that process. Always
	// present in JSON so a consumer that requires a verified claim can see
	// an explicit false. A legacy row is never verified.
	SessionPIDVerified bool `json:"session_pid_verified"`
	// Agent is the declared agent name; always present in JSON, "" when
	// anonymous or legacy.
	Agent string `json:"agent"`
	Cwd   string `json:"cwd"`
	// SessionID is the harness session id, when recorded.
	SessionID string `json:"session_id,omitempty"`
	// Liveness is OwnerLiveness's verdict at list time.
	Liveness Liveness `json:"liveness"`
	// RegisteredAt is the record's own timestamp, or the file's mtime when
	// the record is malformed.
	RegisteredAt string `json:"registered_at"`
	// Legacy marks a v1 file: keyed on a dead command pid, with no agent
	// name and no socket identity. Its liveness is judged by the derived
	// owner pid alone, so it can say a session EXISTS but never WHO it is.
	// Identity-sensitive consumers should treat a legacy row as
	// indeterminate, not as an anonymous session.
	Legacy bool `json:"legacy,omitempty"`
	// Malformed marks a file that is not parseable as a registration. It
	// is listed rather than hidden (a hidden file is one nobody prunes),
	// with Liveness unknown, so only an explicit age bound removes it.
	Malformed bool `json:"malformed,omitempty"`
	// MessagingSocketPath is the recorded socket path, for humans.
	MessagingSocketPath string `json:"messaging_socket_path,omitempty"`
	// Path is the record's file.
	Path string `json:"path"`
}

// SessionsDir returns ~/.pad/sessions, creating it if it doesn't exist.
func SessionsDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	dir := filepath.Join(homeDir, ".pad", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create sessions directory: %w", err)
	}
	// MkdirAll applies the mode only on creation; a directory that already
	// existed with a looser mode keeps it. The registry's records are
	// 0600 each, but a permissive directory would let another local user
	// see the filenames (pids) and plant files — tighten it every time
	// (codex round 3). Best effort: the caller's own directory.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0700 {
		_ = os.Chmod(dir, 0700)
	}
	return dir, nil
}

// registryFileName matches registry records — and ONLY them. The same
// directory holds arm-state files (arm-*.json), atomic-write temp files
// (.arm-*.tmp) and the mutation lock (.lock), which are not sessions.
var registryFileName = regexp.MustCompile(`^[0-9]+\.json$`)

// sessionsLockName is the advisory lock file register and prune hold while
// mutating the directory (see lockSessionsDir).
const sessionsLockName = ".lock"

// RegisterSession records the CURRENT session — owner from the
// environment (CaptureSessionOwner), the given cwd and agent name — to
// ~/.pad/sessions/<session_pid>.json, mode 0600, atomically. Re-registering
// the same session overwrites the record (a refresh). It also prunes DEAD
// records on the way, best effort (the keying fix and the reaping are one
// mechanism: a registry that overwrites instead of accumulating still
// needs someone to remove the records of sessions that ended).
func RegisterSession(cwd, agent string) (SessionRecord, error) {
	dir, err := SessionsDir()
	if err != nil {
		return SessionRecord{}, err
	}
	owner, err := CaptureSessionOwner()
	if err != nil {
		return SessionRecord{}, err
	}
	reg := SessionRegistration{
		SessionOwner: owner,
		SessionID:    firstEnv("PAD_SESSION_ID", "CLAUDE_CODE_SESSION_ID"),
		Agent:        agent,
		RegistrarPID: os.Getpid(),
		Cwd:          cwd,
		// Nanosecond precision so two registrations in one second still
		// order (codex round 2): "newest first" must not fall back to
		// filename order. v1 wrote whole seconds; both parse as RFC3339.
		RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return SessionRecord{}, fmt.Errorf("marshal session registration: %w", err)
	}
	unlock, err := lockSessionsDir(dir)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("lock sessions directory: %w", err)
	}
	defer unlock()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", reg.PID))
	if err := atomicWriteFile(path, data, 0600); err != nil {
		return SessionRecord{}, fmt.Errorf("write session registration: %w", err)
	}
	// Best effort, and only DEAD records: unknown ones need an explicit
	// age bound (PruneSessions), which register has no business choosing.
	// Under the same lock as the write, so no other mutator interleaves.
	_, _ = pruneLocked(0, time.Now())
	return recordFor(path, reg), nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// ListSessions reads every registry record and returns them with a
// liveness verdict each, newest first. A malformed file is returned as a
// Malformed record rather than an error, so one bad file does not hide
// the rest — and so it can be pruned.
func ListSessions() ([]SessionRecord, error) {
	dir, err := SessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read sessions directory: %w", err)
	}
	var out []SessionRecord
	for _, e := range entries {
		// Regular files only: a symlink under a session-shaped name would
		// read some other file as a record (duplicating a live row, or
		// injecting one), and a directory is not a file at all.
		if !e.Type().IsRegular() || !registryFileName.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		rec, err := readSessionRecord(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // removed between ReadDir and read — a concurrent prune
			}
			return nil, err
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Compare as TIMES, not strings: v1 whole-second and v2 nanosecond
		// stamps interleave, and "…:08Z" vs "…:08.5Z" would string-sort the
		// later one first. Ties (identical instants) and unparseable
		// stamps fall to path order, which at least is deterministic.
		ti, ei := time.Parse(time.RFC3339, out[i].RegisteredAt)
		tj, ej := time.Parse(time.RFC3339, out[j].RegisteredAt)
		if ei == nil && ej == nil && !ti.Equal(tj) {
			return ti.After(tj)
		}
		if (ei == nil) != (ej == nil) {
			return ei == nil // parseable before unparseable
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// readSessionRecord reads one file and judges it. Only a missing file is
// an error; unreadable or unparseable content is a Malformed record.
func readSessionRecord(path string) (SessionRecord, error) {
	data, err := readRegistryBytes(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionRecord{}, err
		}
		return malformedRecord(path), nil
	}
	var reg SessionRegistration
	// Invalid UTF-8 is rejected outright: encoding/json would otherwise
	// replace the bad bytes with U+FFFD and hand back an agent name or cwd
	// that is not what the file says (codex round 8).
	if !utf8.Valid(data) {
		return malformedRecord(path), nil
	}
	if err := json.Unmarshal(data, &reg); err != nil || !registrationWellFormed(&reg) {
		return malformedRecord(path), nil
	}
	return recordFor(path, reg), nil
}

// maxRegistryRecordBytes bounds what a registry read will load. A real
// record is well under 1 KiB (a handful of short fields); the bound is
// generous so no legitimate writer approaches it, and it turns a
// multi-megabyte file under a session-shaped name into a malformed row
// instead of a memory bill at every monitor start (codex round 8).
const maxRegistryRecordBytes = 64 * 1024

var (
	errRegistryNotRegular = errors.New("registry entry is not a regular file")
	errRegistryTooLarge   = errors.New("registry entry exceeds the record size bound")
)

// readRegistryBytes opens a registry entry WITHOUT following symlinks or
// blocking on a FIFO (where the platform allows), re-checks that what it
// opened is a regular file — ListSessions filtered on the directory
// entry's type, but a same-user process could swap the name between that
// check and this open — and reads at most the record bound.
func readRegistryBytes(path string) ([]byte, error) {
	f, err := openRegistryFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errRegistryNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRegistryRecordBytes {
		return nil, errRegistryTooLarge
	}
	return data, nil
}

// registrationWellFormed rejects files that parse as JSON but are not
// something this code wrote (codex round 1 P2): `{}` would otherwise read
// as a dead legacy record (owner pid 0) and be pruned with no age bound,
// and `{"session_pid":1}` as a live one. Every record carries a cwd, a
// positive registrar pid and an RFC3339 registered_at; session_pid is
// positive for a v2 record and absent for a v1 one — and since JSON
// cannot tell an absent zero from an explicit one, an explicit
// session_pid:0 is accepted and read as legacy by recordFor (which then
// strips the name such a file cannot legitimately carry). A negative
// session_pid, or a missing field, is malformed — listed, unknown, and
// removed only under an explicit age bound.
func registrationWellFormed(reg *SessionRegistration) bool {
	// Fields every writer (v1 and v2) always emitted: a cwd, a positive
	// registrar pid, an RFC3339 timestamp.
	if reg.RegisteredAt == "" || reg.Cwd == "" || reg.RegistrarPID <= 0 {
		return false
	}
	if _, err := time.Parse(time.RFC3339, reg.RegisteredAt); err != nil {
		return false
	}
	// v2 has a positive owner pid; v1 has none (zero). Negative is a file
	// nobody wrote.
	return reg.PID >= 0
}

// malformedRecord describes a file this code cannot read as a
// registration: liveness unknown (nothing to probe), timestamp from the
// file's mtime so an age bound can still reason about it.
func malformedRecord(path string) SessionRecord {
	rec := SessionRecord{Path: path, Liveness: LivenessUnknown, Malformed: true}
	if info, err := os.Stat(path); err == nil {
		rec.RegisteredAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	return rec
}

// recordFor projects a registration into a record, computing liveness.
func recordFor(path string, reg SessionRegistration) SessionRecord {
	rec := SessionRecord{
		SessionPID:          reg.PID,
		SessionPIDSource:    reg.PIDSource,
		SessionPIDVerified:  reg.PIDVerified,
		Agent:               reg.Agent,
		Cwd:                 reg.Cwd,
		SessionID:           reg.SessionID,
		RegisteredAt:        reg.RegisteredAt,
		MessagingSocketPath: reg.Socket,
		Path:                path,
	}
	owner := reg.SessionOwner
	if reg.PID == 0 {
		// A v1 file — or a crafted file with an explicit session_pid of 0,
		// which JSON cannot tell from an omitted one. Either way the row
		// is legacy: it carries no verifiable owner, so it carries no
		// agent name and no session id either (codex round 3 — a legacy
		// row must never present a name it cannot have recorded).
		rec.Legacy = true
		rec.Agent, rec.SessionID, rec.SessionPIDVerified = "", "", false
		owner = legacyOwner(reg)
		rec.SessionPID, rec.SessionPIDSource = owner.PID, owner.PIDSource
	}
	rec.Liveness = OwnerLiveness(&owner)
	return rec
}

// legacyOwner derives an owner for a v1 file, which recorded the dead
// command pid and a bare socket path. The socket basename is the harness
// pid (Claude Code names its socket <pid>.sock — the read the boot ritual
// used to do by hand), so that pid is the owner when it parses; otherwise
// the registrar pid is all there is. Either way the socket is NOT used as
// the liveness signal: v1 recorded no identity for it, and the
// socket-without-identity rule would judge every legacy record dead — a
// live session's stale v1 files would then be reaped while the session
// runs, which is fine for the files but reads as "no such session" to a
// consumer listing by liveness. Pid-only is the honest verdict: it can
// say the session exists, and the Legacy flag says it cannot say who.
func legacyOwner(reg SessionRegistration) SessionOwner {
	if reg.Socket != "" {
		base := strings.TrimSuffix(filepath.Base(reg.Socket), filepath.Ext(reg.Socket))
		if n, err := strconv.Atoi(base); err == nil && n > 0 {
			return SessionOwner{PID: n, PIDSource: "legacy-socket"}
		}
	}
	return SessionOwner{PID: reg.RegistrarPID, PIDSource: "legacy-registrar"}
}

// PruneReport is what PruneSessions did.
type PruneReport struct {
	// Removed lists the records deleted, in list order.
	Removed []SessionRecord `json:"removed"`
	// DeadRemoved / UnknownRemoved partition Removed by why.
	DeadRemoved    int `json:"dead_removed"`
	UnknownRemoved int `json:"unknown_removed"`
	// Kept counts records left in place (alive, plus unknown ones not
	// covered by the age bound).
	Kept int `json:"kept"`
}

// PruneSessions removes DEAD records, and — only when olderThan > 0 —
// UNKNOWN records whose timestamp is older than now-olderThan. It never
// removes an ALIVE record. The age bound is the only way a registry on a
// platform that cannot probe pids ever shrinks, and it is deliberately an
// explicit choice: register passes 0.
//
// Mutations are serialized under the directory lock (lockSessionsDir), so
// a register cannot rename a fresh record into place between a prune's
// read and its remove; each candidate is additionally RE-READ immediately
// before removal and kept if alive by then (the same non-destructive reap
// as reapArmFile) — which is what stands alone on platforms without the
// lock.
func PruneSessions(olderThan time.Duration, now time.Time) (PruneReport, error) {
	dir, err := SessionsDir()
	if err != nil {
		return PruneReport{}, err
	}
	unlock, err := lockSessionsDir(dir)
	if err != nil {
		return PruneReport{}, fmt.Errorf("lock sessions directory: %w", err)
	}
	defer unlock()
	return pruneLocked(olderThan, now)
}

// pruneLocked is PruneSessions's body; the caller holds the directory lock.
func pruneLocked(olderThan time.Duration, now time.Time) (PruneReport, error) {
	records, err := ListSessions()
	if err != nil {
		return PruneReport{}, err
	}
	var rep PruneReport
	for _, rec := range records {
		remove := false
		switch rec.Liveness {
		case LivenessDead:
			remove = true
		case LivenessUnknown:
			if olderThan > 0 {
				if t, err := time.Parse(time.RFC3339, rec.RegisteredAt); err == nil && now.Sub(t) > olderThan {
					remove = true
				}
			}
		}
		if !remove {
			rep.Kept++
			continue
		}
		if fresh, err := readSessionRecord(rec.Path); err == nil && fresh.Liveness == LivenessAlive {
			rep.Kept++
			continue // re-registered under us — leave it
		}
		if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rep, fmt.Errorf("remove %s: %w", rec.Path, err)
		}
		rep.Removed = append(rep.Removed, rec)
		if rec.Liveness == LivenessDead {
			rep.DeadRemoved++
		} else {
			rep.UnknownRemoved++
		}
	}
	return rep, nil
}
