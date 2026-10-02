package server

import (
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// workspace_access_changed publishers (TASK-3272, PLAN-3002 U7a).
//
// Every door that gives a user access to a workspace, takes it away, or
// deletes, restores or purges the workspace itself publishes one
// notification per affected user on the user stream, AFTER its write has
// committed. The event is a refetch hint for the web tab bar: correctness
// lives on the read-filtered GET /me/workspace-tabs, so a duplicate or a
// missed event under concurrent grant writes is benign and nothing here
// locks to make it exact. The door x recipient table is checkpoint 3 on
// TASK-3272's trail.
//
// Two doors grant access and deliberately publish nothing: cloud
// autoCreateWorkspace and register-with-invitation (handlers_auth.go). Every
// caller of either acts on an account created in that same request, which
// cannot yet hold a stream to receive the event, and register can still
// delete that account after its membership add. A door that grants access to
// an EXISTING account must publish.

// publishWorkspaceAccessChanged publishes one notification per user. Actor
// and actorName describe who caused the change; a request-less caller (the
// scheduled purge) passes "system" and "".
func (s *Server) publishWorkspaceAccessChanged(workspaceID, change string, userIDs []string, actor, actorName string) {
	// TASK-3365: every door that changes whether a user reaches a workspace
	// comes through here, so it kicks their live connections to re-check
	// NOW, bus or no bus. Lifecycle changes kick the whole workspace too, which
	// reaches legacy workspace-token streams that have no user.
	for _, uid := range userIDs {
		s.invalidateUserAccess(uid)
	}
	switch change {
	case watchevents.AccessDeleted, watchevents.AccessRestored, watchevents.AccessPurged:
		s.invalidateWorkspaceAccess(workspaceID)
	}
	if s.watchEvents == nil {
		return
	}
	seen := make(map[string]bool, len(userIDs))
	for _, uid := range userIDs {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		s.publishWatchNotification(watchevents.Notification{
			WorkspaceID:  workspaceID,
			Kind:         watchevents.KindWorkspaceAccessChanged,
			AccessChange: change,
			TargetUserID: uid,
			Actor:        actor,
			ActorName:    actorName,
			Summary:      "workspace access " + change,
		})
	}
}

// publishWorkspaceAccessChangedFromRequest is publishWorkspaceAccessChanged
// with the actor read off the request that made the change.
func (s *Server) publishWorkspaceAccessChangedFromRequest(r *http.Request, workspaceID, change string, userIDs ...string) {
	actor, _ := actorFromRequest(r)
	s.publishWorkspaceAccessChanged(workspaceID, change, userIDs, actor, actorNameFromRequest(r))
}

// userReachesWorkspace reports whether a user reaches a workspace as a
// member or through any live grant: the question a grant or member write
// asks before and after itself to decide whether access was gained or lost.
func (s *Server) userReachesWorkspace(workspaceID, userID string) (bool, error) {
	m, err := s.store.GetWorkspaceMember(workspaceID, userID)
	if err != nil {
		return false, err
	}
	if m != nil {
		return true, nil
	}
	return s.store.UserHasGrantsInWorkspace(workspaceID, userID)
}

// workspaceAccessUsers reads the users who reach a workspace, for a
// lifecycle door that must read them BEFORE its own write (a purge deletes
// the rows). A failed read is logged and yields nobody: the write it
// precedes must not fail for want of a hint.
func (s *Server) workspaceAccessUsers(workspaceID string) []string {
	ids, err := s.store.ListWorkspaceAccessUserIDs(workspaceID)
	if err != nil {
		slog.Warn("workspace_access_changed: could not read the workspace's users; no notification will be sent",
			"workspace_id", workspaceID, "error", err)
		return nil
	}
	return ids
}

// publishLostIfUnreachable publishes "lost" for a user after a per-user loss
// write (member removal, grant revoke) when nothing is left that reaches the
// workspace: a member removed while keeping a grant, or a guest losing one of
// two grants, still reaches it. A failed read publishes anyway, because the
// event only asks the client to refetch.
func (s *Server) publishLostIfUnreachable(r *http.Request, workspaceID, userID string) {
	// TASK-3365: a removal or revoke that leaves the user SOME access still
	// narrows it, so their connections re-check whether or not "lost" goes
	// out below.
	s.invalidateUserAccess(userID)
	reaches, err := s.userReachesWorkspace(workspaceID, userID)
	if err == nil && reaches {
		return
	}
	s.publishWorkspaceAccessChangedFromRequest(r, workspaceID, watchevents.AccessLost, userID)
}
