package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// handlePlaybookLibrary returns the global playbook library.
//
// Query params (all optional):
//   - category=<name> — return only the matching category. Case-sensitive
//     exact match against PlaybookCategory.Name. Unknown categories return
//     an empty Categories slice.
//   - summary=true   — strip Content from each LibraryPlaybook and inject
//     a Summary field via collections.PlaybookSummary. Keeps payloads
//     compact for browsing surfaces (CLI default, MCP). Web UI and other
//     consumers that want the full body omit the flag.
//
// TASK-1561 / PLAN-1560. No workspace context — the library is global.
func (s *Server) handlePlaybookLibrary(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}

	cats := collections.PlaybookLibrary()

	if category := r.URL.Query().Get("category"); category != "" {
		filtered := make([]collections.PlaybookCategory, 0, 1)
		for _, cat := range cats {
			if cat.Name == category {
				filtered = append(filtered, cat)
			}
		}
		cats = filtered
	}

	if r.URL.Query().Get("summary") == "true" {
		// Materialize a summary-mode copy. Categories carry value-type
		// playbook slices, so we can mutate freely after copying without
		// touching the package-level library data.
		summarized := make([]collections.PlaybookCategory, len(cats))
		for i, cat := range cats {
			pbs := make([]collections.LibraryPlaybook, len(cat.Playbooks))
			for j, pb := range cat.Playbooks {
				pb.Summary = collections.PlaybookSummary(pb.Content)
				pb.Content = ""
				pbs[j] = pb
			}
			cat.Playbooks = pbs
			summarized[i] = cat
		}
		cats = summarized
	}

	writeJSON(w, http.StatusOK, response{Categories: cats})
}
