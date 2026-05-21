package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// libraryEntryResponse is the envelope returned by the /library/entry
// endpoint — type is "convention" or "playbook" so callers can deserialize
// the entry into the right shape without inspecting fields. Only one of
// Convention or Playbook is set per response.
type libraryEntryResponse struct {
	Type       string                         `json:"type"`
	Convention *collections.LibraryConvention `json:"convention,omitempty"`
	Playbook   *collections.LibraryPlaybook   `json:"playbook,omitempty"`
}

// handleLibraryEntry returns a single library entry by exact title match.
//
// Lookup precedence — conventions first, then playbooks — mirrors the
// dispatcher's `library activate` so the two stay in lockstep: if a title
// resolves to a convention for activate, it resolves to a convention here
// too.
//
// Required query param:
//   - title=<exact-title>
//
// Returns 400 if title is missing, 404 if not found in either library.
// Full body is included; this endpoint is the canonical "get one entry's
// full content" path complementing the list endpoints' summary mode.
//
// TASK-1561 / PLAN-1560. No workspace context — the library is global.
func (s *Server) handleLibraryEntry(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "title query parameter is required",
		})
		return
	}

	if conv := collections.GetLibraryConvention(title); conv != nil {
		writeJSON(w, http.StatusOK, libraryEntryResponse{
			Type:       "convention",
			Convention: conv,
		})
		return
	}

	if pb := collections.GetLibraryPlaybook(title); pb != nil {
		writeJSON(w, http.StatusOK, libraryEntryResponse{
			Type:     "playbook",
			Playbook: pb,
		})
		return
	}

	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "not found in convention or playbook library: " + title,
	})
}
