package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// BUG-3184: the server-wide ReadTimeout (15s, set in Listen) bounds reading
// the WHOLE request body, measured from the start of the request. It is the
// slowloris defence: it limits how long a client may take to send. But the
// workspace import reads its body WHILE it writes (the bundle path imports
// pad-export.json and then streams each blob), so time the SERVER spends
// between reads, in ImportWorkspace, on database contention, on a slow disk,
// counts against it too. Any import still reading at 15s failed mid-body,
// whatever the size of the bundle (2 of 8 local e2e runs on a 97 KB bundle;
// the reproduction tables are on BUG-3184's trail).
//
// So the import route arms the connection's read deadline before EACH Read
// instead: now + idle, never past the ceiling. The deadline then measures only
// time spent waiting for the client inside a Read, which is what the global
// timeout is for, and server work between reads no longer counts. The ceiling
// bounds a client that trickles a byte just inside every idle window.
const (
	defaultImportReadIdle    = 60 * time.Second
	defaultImportReadCeiling = time.Hour
)

// withImportReadDeadline replaces r.Body with a reader that arms a per-Read
// deadline. Call it only once the caller is authorised to mint a workspace
// (after beginWorkspaceMint), so an unauthenticated slow client still meets
// the server-wide 15s bound.
//
// When the connection cannot take a deadline (http.ErrNotSupported, e.g. a
// ResponseRecorder or a wrapped writer), the body is left alone and the
// server-wide deadline stays in force: that is today's behaviour, never worse.
func (s *Server) withImportReadDeadline(w http.ResponseWriter, r *http.Request) {
	idle := s.importReadIdle
	if idle <= 0 {
		idle = defaultImportReadIdle
	}
	ceiling := s.importReadCeiling
	if ceiling <= 0 {
		ceiling = defaultImportReadCeiling
	}
	rc := http.NewResponseController(w)
	until := time.Now().Add(ceiling)
	if err := rc.SetReadDeadline(time.Now().Add(idle)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			slog.Warn("import: could not extend the read deadline; the server-wide one applies",
				"error", err)
		}
		return
	}
	r.Body = &perReadDeadlineBody{ReadCloser: r.Body, rc: rc, idle: idle, until: until}
}

type perReadDeadlineBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	idle  time.Duration
	until time.Time
}

func (b *perReadDeadlineBody) Read(p []byte) (int, error) {
	d := time.Now().Add(b.idle)
	if d.After(b.until) {
		d = b.until
	}
	// Supported once is supported for the connection; an error here would
	// leave the previous deadline armed, which is still a bound.
	_ = b.rc.SetReadDeadline(d)
	return b.ReadCloser.Read(p)
}
