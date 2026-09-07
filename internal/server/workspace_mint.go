package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Workspace minting has TWO doors — handleCreateWorkspace and
// handleImportWorkspace, the latter with a second body shape behind it
// (handleImportWorkspaceBundle) — and both reach store.CreateWorkspace.
// Every precondition one enforces, the other must enforce, FROM HERE
// (BUG-2809).
//
// The record is why this exists rather than a note asking people to
// remember: two preconditions diverged and were fixed one at a time, each
// found by a reviewer rather than by the door that lacked it — the OAuth
// consent grant (IDEA-2756) and the user-scoped workspace plan limit
// (BUG-2793). A third divergence was live when this landed: import
// accepted an empty workspace name, and an empty name slugifies to an
// EMPTY SLUG, which is a routing key. Measured on that tip: the first such
// import took the empty slug and a second one landed on "-2".
//
// The preconditions split by WHEN they can run, and that split is
// load-bearing rather than tidy:
//
//   - beginWorkspaceMint runs BEFORE the body is read, so a refused caller
//     never uploads a bundle, and so a refusal cannot be probed by body
//     shape. It also resolves the two things a mint is attributed with.
//   - validateWorkspaceMintPayload runs AFTER, because its subject is the
//     payload. It returns an error rather than writing one: the JSON doors
//     answer 400 `bad_request` and the bundle door answers 400 `bad_bundle`
//     through importStatusError, so the RULE is shared and the envelope
//     stays each door's own.

// workspaceMintAuth carries what a mint door resolves from the REQUEST, as
// opposed to from its payload. Both fields are authoritative here and must
// never be read from a request body: Source decides UI affordances
// (BUG-1557) and OwnerID decides who the workspace belongs to.
type workspaceMintAuth struct {
	// OwnerID is the authenticated user, or "" for a caller TokenAuth
	// admits with no resolved user (a legacy workspace token, or the
	// fresh-install window). See the quota note in beginWorkspaceMint.
	OwnerID string
	// Source is derived from the auth shape: "cli" for bearer/api-token
	// callers, "web" for cookie sessions.
	Source string
}

// beginWorkspaceMint runs every precondition that does not need the body,
// and returns false having already written the response when one refuses.
//
// Call it FIRST in any handler that mints a workspace — above the body
// read, and for the import door above the Content-Type dispatch, so one
// line covers both body shapes.
func (s *Server) beginWorkspaceMint(w http.ResponseWriter, r *http.Request) (workspaceMintAuth, bool) {
	// Consent: the OAuth connection's may_create_workspaces grant
	// (IDEA-2756). First, so a refusal never depends on body validity.
	if !s.requireWorkspaceCreationConsent(w, r) {
		return workspaceMintAuth{}, false
	}

	userID := currentUserID(r)

	// Plan limit, user-scoped (BUG-2793). An import IS a new workspace and
	// counts, with no exemption for re-importing something you previously
	// owned — export provenance is not trustworthy enough to gate billing
	// on, and the at-limit case that deserves relief (undoing a delete) is
	// served by the restore endpoint, which mints nothing.
	//
	// The `userID != ""` guard is not defensive padding: a legacy workspace
	// token resolves no user, and charging an unattributable mint against
	// nobody's plan is not a limit. Whether such a caller should be able to
	// mint an UNOWNED workspace at all is a live question on BUG-2809's
	// trail, deliberately not decided here — this function preserves the
	// behaviour both doors already had rather than changing it under cover
	// of a refactor.
	if userID != "" {
		if !s.enforceUserPlanLimit(w, userID, "workspaces") {
			return workspaceMintAuth{}, false
		}
	}

	_, source := actorFromRequest(r)
	return workspaceMintAuth{OwnerID: userID, Source: source}, true
}

// errWorkspaceNameRequired is the empty-name refusal, exported as a value so
// a door can tell it from a settings failure without matching on prose.
var errWorkspaceNameRequired = errors.New("Name is required")

// validateWorkspaceMintPayload enforces the preconditions whose subject is
// the payload, and NORMALIZES the settings in place when they are valid.
//
// name is the EFFECTIVE name — for an import that is the ?name= override
// when one was given, otherwise the bundle's own — because the effective
// name is what becomes the slug.
//
// settings is a pointer so a valid blob is written back normalized; pass
// nil when the door has no settings to offer. Rejecting malformed settings
// HERE is what makes the two doors agree on the status code: the store
// normalizes too (createWorkspaceQ), but a store error surfaces as 500
// import_failed, and caller-supplied malformed JSON is a 400 on the create
// door. Same input, same answer, whichever door it arrives at.
func validateWorkspaceMintPayload(name string, settings *string) error {
	if name == "" {
		return errWorkspaceNameRequired
	}
	if settings != nil && *settings != "" {
		normalized, err := models.NormalizeWorkspaceSettings(*settings)
		if err != nil {
			return fmt.Errorf("invalid settings JSON: %w", err)
		}
		*settings = normalized
	}
	return nil
}
