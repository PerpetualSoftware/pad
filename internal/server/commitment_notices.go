package server

import (
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// afterCommitmentRefusal runs only after the refused transaction has rolled
// back. Notice delivery never changes the refusal or logs private graph data.
func (s *Server) afterCommitmentRefusal(r *http.Request, workspaceID string, in models.LinkedMoveInput) {
	if in.Preview {
		return
	}
	in.ActorUserID = currentUserID(r)
	enqueue := s.store.EnqueueLinkedDeadlineApprovalNotice
	if s.enqueueCommitmentRefusal != nil {
		enqueue = s.enqueueCommitmentRefusal
	}
	if err := enqueue(workspaceID, in); err != nil {
		slog.Warn("enqueue_commitment_refusal", "error_class", "storage")
	}
}
