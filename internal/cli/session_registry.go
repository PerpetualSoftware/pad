package cli

// On-disk session registry (TASK-2533; reworked in TASK-2767 for IDEA-2750
// part 2).
//
// ~/.pad/sessions/<session_pid>.json — one record per SESSION, keyed on
// the OWNER pid (the agent harness's session process, see SessionOwner),
// carrying the agent name the session declared. `pad session register`
// writes it; `pad session list` reads it with a liveness verdict per row;
// `pad session prune` removes the dead ones. The plugin monitor registers
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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	return dir, nil
}

// registryFileName matches registry records — and ONLY them. The same
// directory holds arm-state files (arm-*.json) and atomic-write temp
// files (.arm-*.tmp), which are not sessions.
var registryFileName = regexp.MustCompile(`^[0-9]+\.json$`)

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
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return SessionRecord{}, fmt.Errorf("marshal session registration: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", reg.PID))
	if err := atomicWriteFile(path, data, 0600); err != nil {
		return SessionRecord{}, fmt.Errorf("write session registration: %w", err)
	}
	// Best effort, and only DEAD records: unknown ones need an explicit
	// age bound (PruneSessions), which register has no business choosing.
	_, _ = PruneSessions(0, time.Now())
	return recordFor(path, reg, nil), nil
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
		if e.IsDir() || !registryFileName.MatchString(e.Name()) {
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
		return out[i].RegisteredAt > out[j].RegisteredAt // RFC3339 UTC sorts lexically
	})
	return out, nil
}

// readSessionRecord reads one file and judges it. Only a missing file is
// an error; unreadable or unparseable content is a Malformed record.
func readSessionRecord(path string) (SessionRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionRecord{}, err
		}
		return malformedRecord(path), nil
	}
	var reg SessionRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		return malformedRecord(path), nil
	}
	return recordFor(path, reg, nil), nil
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
// liveness may be pre-supplied by a caller that already knows it (a
// register that just wrote the file); nil computes it.
func recordFor(path string, reg SessionRegistration, liveness *Liveness) SessionRecord {
	rec := SessionRecord{
		SessionPID:          reg.PID,
		SessionPIDSource:    reg.PIDSource,
		Agent:               reg.Agent,
		Cwd:                 reg.Cwd,
		SessionID:           reg.SessionID,
		RegisteredAt:        reg.RegisteredAt,
		MessagingSocketPath: reg.Socket,
		Path:                path,
	}
	owner := reg.SessionOwner
	if reg.PID == 0 {
		rec.Legacy = true
		owner = legacyOwner(reg)
		rec.SessionPID, rec.SessionPIDSource = owner.PID, owner.PIDSource
	}
	if liveness != nil {
		rec.Liveness = *liveness
	} else {
		rec.Liveness = OwnerLiveness(&owner)
	}
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
// Each candidate is RE-READ immediately before removal and kept if it is
// alive by then — a session re-registering into the same file between
// our list and our remove must not lose its fresh record (the same
// non-destructive reap as reapArmFile).
func PruneSessions(olderThan time.Duration, now time.Time) (PruneReport, error) {
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
