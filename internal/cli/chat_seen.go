package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// The chat-seen log (TASK-4071) is how a chat line delivered to an agent
// also reaches the human at that agent's terminal. `pad watch --stream
// --for-session` appends every chat line it prints to stdout, byte for byte,
// to ~/.pad/chat-seen/<harness-pid>.log; the Claude Code plugin tails it
// into a band above the prompt, and any other harness can `tail -F` it.
//
// It is keyed by the HARNESS pid (SessionOwner.PID), the one identity that
// both writers (pad watch, from PAD_SESSION_PID or CLAUDE_PID) and the
// plugin's reader (its sessionPid()) know, and that survives a /clear.
//
// It is a display cue, never a record: the agent's stdout is the record.
// Rotation can therefore lose display only. A reader that polls between two
// rotations misses the lines that moved into <pid>.log.1, and nothing
// catches them up (TASK-4071 C3, lead ruling day 35).

// ChatSeenMaxBytes caps one generation of a session's chat-seen log. On the
// write that would pass it, the log moves to <pid>.log.1 (replacing an older
// one) and a fresh log starts, so a session holds at most two generations.
const ChatSeenMaxBytes = 256 << 10

// ensurePrivateDir creates dir at 0700, and tightens it to 0700 when it
// already existed looser: MkdirAll applies the mode only on creation.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0700 {
		_ = os.Chmod(dir, 0700)
	}
	return nil
}

// chatSeenDirPath is ~/.pad/chat-seen, without creating it.
func chatSeenDirPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pad", "chat-seen"), nil
}

// ChatSeenPath is the log for one harness pid.
func ChatSeenPath(pid int) (string, error) {
	dir, err := chatSeenDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(pid)+".log"), nil
}

// removeChatSeen deletes both generations of a pid's log. It never creates
// the directory, and a missing file is not an error.
func removeChatSeen(pid int) error {
	if pid <= 0 {
		return nil
	}
	path, err := ChatSeenPath(pid)
	if err != nil {
		return err
	}
	for _, p := range []string{path, path + ".1"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ChatSeenLog appends chat lines to one session's log. A nil *ChatSeenLog is
// valid and writes nothing. Any failure is reported once on warn and turns
// the log off for the rest of the process; it never affects stdout.
type ChatSeenLog struct {
	mu       sync.Mutex
	path     string
	max      int64
	warn     io.Writer
	f        *os.File
	size     int64
	disabled bool
	// off is why this process keeps no log at all. It is reported on the
	// first line that would have been written, not at start: the monitor
	// prints nothing until it has something to deliver (DOC-2479).
	off string
}

// OpenChatSeen decides whether this process keeps a chat-seen log. It
// writes nothing yet: the file is created on the first line, so a session
// that never receives chat leaves no file, and a log that is off says so on
// that first line too.
//
// A pid that came from os.Getpid() (PIDSource "self", neither
// PAD_SESSION_PID nor CLAUDE_PID set) disables it (TASK-4071 C2): the log
// would be keyed by pad watch's own pid, which no reader looks for and no
// session record names, so nothing would ever read or prune it.
func OpenChatSeen(owner SessionOwner, ownerErr error, warn io.Writer) *ChatSeenLog {
	l := &ChatSeenLog{max: ChatSeenMaxBytes, warn: warn}
	switch {
	case ownerErr != nil:
		l.off = ownerErr.Error()
	case owner.PIDSource == "self" || owner.PID <= 0:
		l.off = "no harness pid (set PAD_SESSION_PID to the agent harness's pid)"
	default:
		path, err := ChatSeenPath(owner.PID)
		if err != nil {
			l.off = err.Error()
		}
		l.path = path
	}
	return l
}

// Path is the log's path, or "" when there is none.
func (l *ChatSeenLog) Path() string {
	if l == nil || l.off != "" {
		return ""
	}
	return l.path
}

// Append writes line plus a newline: the same bytes the caller printed.
func (l *ChatSeenLog) Append(line string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled {
		return
	}
	if l.off != "" {
		l.disabled = true
		fmt.Fprintf(l.warn, "Pad chat: not showing chat lines to the human: %s.\n", l.off)
		return
	}
	if err := l.append(line + "\n"); err != nil {
		l.disabled = true
		if l.f != nil {
			_ = l.f.Close()
			l.f = nil
		}
		fmt.Fprintf(l.warn, "Pad chat: stopped showing chat lines to the human: %s\n", err)
	}
}

func (l *ChatSeenLog) append(data string) error {
	// A log removed under us (pad session prune --older-than on a record
	// whose liveness was unknown) would otherwise take every later line
	// into an unlinked file nobody can read: reopen at the path instead.
	if l.f != nil && !l.stillAtPath() {
		_ = l.f.Close()
		l.f = nil
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return err
		}
	}
	n := int64(len(data))
	if l.size > 0 && l.size+n > l.max {
		// C3: lines a reader has not polled yet move to .1 and are not
		// shown. The agent's stdout already carried them.
		_ = l.f.Close()
		l.f = nil
		// Rename does not replace an existing file on Windows.
		if err := os.Remove(l.path + ".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate %s: %w", l.path, err)
		}
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return fmt.Errorf("rotate %s: %w", l.path, err)
		}
		if err := l.open(); err != nil {
			return err
		}
	}
	w, err := l.f.WriteString(data)
	l.size += int64(w)
	if err != nil {
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	return nil
}

// stillAtPath reports whether the open file is still the one at l.path.
func (l *ChatSeenLog) stillAtPath() bool {
	open, err := l.f.Stat()
	if err != nil {
		return false
	}
	at, err := os.Lstat(l.path)
	return err == nil && os.SameFile(open, at)
}

// open opens (creating) the log 0600 in a 0700 directory. It refuses a
// symlinked directory, a symlink at the log's path and anything but a
// regular file, and tightens a looser file.
func (l *ChatSeenLog) open() error {
	dir := filepath.Dir(l.path)
	// ensurePrivateDir follows a symlink, and would chmod its target.
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dir)
	}
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.OpenFile(l.path, chatSeenOpenFlags, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat %s: %w", l.path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("%s is not a regular file", l.path)
	}
	if info.Mode().Perm() != 0600 {
		if err := f.Chmod(0600); err != nil {
			_ = f.Close()
			return fmt.Errorf("chmod %s: %w", l.path, err)
		}
	}
	l.f, l.size = f, info.Size()
	return nil
}
