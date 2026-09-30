package materialize

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxFrameBytes bounds one worker protocol frame. A length prefix above it
// cannot be skipped safely (the stream is desynchronised or hostile), so the
// worker answers with an error response and stops.
const MaxFrameBytes = 256 << 20

// WorkerRequest is one job on the worker protocol.
type WorkerRequest struct {
	ID uint64 `json:"id"`
	// Rows are base64 op-log frames; see Job.Rows.
	Rows          []string    `json:"rows"`
	SchemaVersion string      `json:"schema_version"`
	LinkIndex     []LinkEntry `json:"link_index"`
	WorkspaceSlug string      `json:"workspace_slug,omitempty"`
	// TimeoutMs bounds the job; 0 means no deadline inside the worker (the
	// supervisor still owns the process).
	TimeoutMs int64 `json:"timeout_ms"`
}

// WorkerResponse answers one WorkerRequest. Exactly one of Markdown and Error
// is set; Markdown may be the empty string (an empty document), which is why
// it is a pointer.
type WorkerResponse struct {
	ID       uint64  `json:"id"`
	Markdown *string `json:"markdown,omitempty"`
	Error    string  `json:"error,omitempty"`
	// Ms is the time the worker spent on the request.
	Ms float64 `json:"ms"`
}

// RunWorker serves the materializer worker protocol: frames of a uint32
// big-endian length followed by that many bytes of JSON, a WorkerRequest in
// on r and a WorkerResponse out on w, one response per request, in order.
//
// A frame that is not a valid request gets an error response (ID 0 when it
// could not be read) and the loop continues. EOF at a frame boundary ends the
// loop cleanly with nil; EOF inside a frame, an oversized length prefix, or a
// write failure ends it with an error. The bundle is loaded once, before the
// first frame is read; a load failure is returned without reading anything.
//
// w is the protocol channel and nothing else may be written to it.
func RunWorker(r io.Reader, w io.Writer, js []byte) error {
	runner, err := New(js)
	if err != nil {
		return err
	}
	br := bufio.NewReader(r)
	bw := bufio.NewWriter(w)
	for {
		payload, err := readFrame(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				_ = writeFrame(bw, WorkerResponse{Error: err.Error()})
			}
			return err
		}
		resp := serve(runner, payload)
		if err := writeFrame(bw, resp); err != nil {
			return err
		}
	}
}

var errFrameTooLarge = fmt.Errorf("materialize worker: frame exceeds %d bytes", MaxFrameBytes)

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("materialize worker: truncated frame header: %w", err)
		}
		return nil, err // io.EOF: clean end
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, errFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("materialize worker: truncated frame body: %w", err)
	}
	return buf, nil
}

func writeFrame(w *bufio.Writer, resp WorkerResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

func serve(runner *Runner, payload []byte) WorkerResponse {
	start := time.Now()
	var req WorkerRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return WorkerResponse{Error: "malformed request: " + err.Error(), Ms: elapsedMs(start)}
	}
	fail := func(msg string) WorkerResponse {
		return WorkerResponse{ID: req.ID, Error: msg, Ms: elapsedMs(start)}
	}
	if req.TimeoutMs < 0 {
		return fail("malformed request: timeout_ms is negative")
	}
	job := Job{Rows: make([][]byte, len(req.Rows)), SchemaVersion: req.SchemaVersion, LinkIndex: req.LinkIndex, WorkspaceSlug: req.WorkspaceSlug}
	for i, s := range req.Rows {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fail(fmt.Sprintf("malformed request: rows[%d] is not base64: %v", i, err))
		}
		job.Rows[i] = b
	}
	ctx := context.Background()
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	md, err := runner.Materialize(ctx, job)
	if err != nil {
		return fail(err.Error())
	}
	return WorkerResponse{ID: req.ID, Markdown: &md, Ms: elapsedMs(start)}
}
