package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ResolveWorkerID decides what goes in the X-Pad-Worker header (TASK-3613):
// the opaque id this session holds item leases under, so two sessions that
// share an agent name and an account do not share a lease.
//
//  1. $PAD_WORKER_ID when it is well formed: an explicit choice, for resuming
//     after a restart or for a harness pad cannot register.
//  2. This session's registry record (`pad session register`), minting and
//     storing an id into a record written before worker ids existed.
//  3. Otherwise "": a name-only caller, today's behaviour. No other
//     persistence is invented (a per-cwd file would be shared by every
//     unregistered session in that checkout, which is the collision again).
func ResolveWorkerID() string {
	if v := strings.TrimSpace(os.Getenv("PAD_WORKER_ID")); models.ValidWorkerID(v) {
		return v
	}
	return sessionWorkerID()
}

// MintWorkerID returns a fresh worker id: "wk_" + 128 random bits in hex.
func MintWorkerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint lease worker id: %w", err)
	}
	return "wk_" + hex.EncodeToString(b[:]), nil
}

// sessionWorkerID returns this session's worker id from its registry record,
// minting one into a record that predates worker ids. "" when the session is
// not registered or the record cannot be updated.
func sessionWorkerID() string {
	path, reg, ok := thisSessionRegistration()
	if !ok {
		return ""
	}
	if models.ValidWorkerID(reg.WorkerID) {
		return reg.WorkerID
	}
	unlock, err := lockSessionsDir(filepath.Dir(path))
	if err != nil {
		return ""
	}
	defer unlock()
	// Re-read under the lock: another `pad` process in this session may
	// have minted one since our first read.
	if id := keptWorkerID(path, reg.SessionOwner); id != "" {
		return id
	}
	id, err := MintWorkerID()
	if err != nil {
		return ""
	}
	reg.WorkerID = id
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return ""
	}
	if err := atomicWriteFile(path, data, 0600); err != nil {
		return ""
	}
	return id
}

// keptWorkerID is the worker id in the record at path when that record
// belongs to the same process as owner (pid AND start token), else "". The
// start token is what tells a /clear in one process from a new process
// that reuses the pid; see below for platforms that record none.
func keptWorkerID(path string, owner SessionOwner) string {
	data, err := readRegistryBytes(path)
	if err != nil {
		return ""
	}
	var prev SessionRegistration
	if json.Unmarshal(data, &prev) != nil || !registrationWellFormed(&prev) {
		return ""
	}
	// Same pid AND same start token is the same process. Where the platform
	// records no start token on either side, the pid alone has to do: a
	// /clear must keep its leases (lead ruling), and a reused pid inheriting
	// a dead session's id is the lesser risk, since its leases expire within
	// their TTL. A token on one side only is a different process.
	if prev.PID != owner.PID || prev.ProcStart != owner.ProcStart {
		return ""
	}
	if !models.ValidWorkerID(prev.WorkerID) {
		return ""
	}
	return prev.WorkerID
}

// SetWorkerID makes this client send id as X-Pad-Worker instead of the one
// ResolveWorkerID would pick: the `--worker-id` flag, which is also how a
// local-stdio MCP call's `worker_id` param arrives. It must be called before
// the first request. An empty id explicitly omits the header without resolving a
// replacement. A malformed non-empty id is refused here, not sent.
func (c *Client) SetWorkerID(id string) error {
	if id != "" && !models.ValidWorkerID(id) {
		return fmt.Errorf(`--worker-id must be "wk_" followed by 32 lowercase hex characters, got %q`, id)
	}
	c.workerOnce.Do(func() {})
	c.workerID = id
	return nil
}
