package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// pidRecord is what `pad server start` leaves behind so `pad server stop` can
// tell OUR server from whatever else happens to hold that pid.
//
// BUG-2969: before this, stop read a bare pid and signalled it. os.FindProcess
// succeeds for any pid on Unix, nothing asked whether the pid was a pad server,
// and the wait loop then confirmed success by polling the PORT — which is
// unhealthy from the first poll when nothing was ever serving. Measured: a
// `sleep 600` whose pid had been written into the file was SIGTERMed, and stop
// printed "Server stopped." The success check was satisfied by the failure case.
//
// The fields divide by who reads them:
//
//   - PID is what gets signalled.
//   - StartedAt and Exe are for a HUMAN opening the file to work out what stop
//     is talking about. On Windows StartedAt is also the discriminator (see
//     pidfile_windows.go); on Unix the discriminator is an advisory lock, not
//     this timestamp, so do not add logic that trusts it there.
type pidRecord struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Exe       string    `json:"exe,omitempty"`
}

// writePIDRecord serialises rec to path.
func writePIDRecord(path string, rec pidRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readPIDRecord parses the PID file.
//
// It accepts the LEGACY bare-integer form as well, because a server started by
// an older binary is exactly the case this whole family of bugs is about: the
// file outlives the build that wrote it. A legacy record carries no fingerprint,
// which the ownership check treats as unprovable rather than as proof.
func readPIDRecord(path string) (rec pidRecord, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pidRecord{}, false
	}
	return parsePIDRecord(data)
}

// readAll reads an already-open PID file from the start. Errors collapse to
// empty, which parsePIDRecord reports as unreadable — the same answer an
// absent file gives, and the same refusal follows from it.
func readAll(f *os.File) []byte {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return data
}

// parsePIDRecord parses PID-file bytes in either form.
func parsePIDRecord(data []byte) (rec pidRecord, ok bool) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return pidRecord{}, false
	}

	if trimmed[0] == '{' {
		if err := json.Unmarshal([]byte(trimmed), &rec); err != nil || rec.PID <= 0 {
			return pidRecord{}, false
		}
		return rec, true
	}

	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return pidRecord{}, false
	}
	return pidRecord{PID: pid}, true
}

// currentPIDRecord describes THIS process.
func currentPIDRecord() pidRecord {
	rec := pidRecord{PID: os.Getpid(), StartedAt: processStartTime(os.Getpid())}
	if exe, err := os.Executable(); err == nil {
		rec.Exe = exe
	}
	return rec
}

// pidFileOwnership is what the platform check reports back to stop.
type pidFileOwnership int

const (
	// pidFileOurs: a live pad server owns this file. Signalling is safe.
	pidFileOurs pidFileOwnership = iota
	// pidFileStale: nothing owns it. Signal NOTHING — the pid may since have
	// been reused by a process that has nothing to do with us.
	pidFileStale
	// pidFileUnprovable: this platform, or this record, cannot answer. Also
	// signal nothing: an unprovable claim is not a licence to send SIGTERM.
	pidFileUnprovable
)

func (o pidFileOwnership) String() string {
	switch o {
	case pidFileOurs:
		return "ours"
	case pidFileStale:
		return "stale"
	default:
		return "unprovable"
	}
}

// describePIDRecord renders a record for an error message a person has to act
// on. Kept here so both platforms word it the same way.
func describePIDRecord(rec pidRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d", rec.PID)
	if !rec.StartedAt.IsZero() {
		fmt.Fprintf(&b, ", recorded at %s", rec.StartedAt.Format(time.RFC3339))
	}
	if rec.Exe != "" {
		fmt.Fprintf(&b, ", %s", rec.Exe)
	}
	return b.String()
}
