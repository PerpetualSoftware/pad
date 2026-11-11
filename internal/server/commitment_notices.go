package server

import (
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// commitmentRefusalAttempt carries only an attempted operation, never an error
// or response value. Exactly one operation is populated by the handler.
type commitmentRefusalAttempt struct {
	move   *models.LinkedMoveInput
	create *models.ScheduleLinkCreate
	linkID string
	update *models.ScheduleLinkUpdate
}

// afterCommitmentRefusal runs only after the refused transaction has rolled
// back. Notice delivery never changes the refusal or logs private graph data.
func (s *Server) afterCommitmentRefusal(r *http.Request, workspaceID string, attempt commitmentRefusalAttempt) {
	var err error
	if attempt.move != nil {
		in := *attempt.move
		if in.Preview {
			return
		}
		in.ActorUserID = currentUserID(r)
		in.CommitmentAccessLoader = s.graphAccessLoader(r, workspaceID, commitmentAccessLive, in.ItemID)
		enqueue := s.store.EnqueueLinkedDeadlineApprovalNotice
		if s.enqueueCommitmentRefusal != nil {
			enqueue = s.enqueueCommitmentRefusal
		}
		err = enqueue(workspaceID, in)
	} else {
		if attempt.create == nil && attempt.update == nil {
			return
		}
		if attempt.create != nil && attempt.create.Move.Preview || attempt.update != nil && attempt.update.Move.Preview {
			return
		}
		enqueue := s.store.EnqueueLinkedWriteDeadlineApprovalNotice
		if s.enqueueCommitmentLinkRefusal != nil {
			enqueue = s.enqueueCommitmentLinkRefusal
		}
		err = enqueue(workspaceID, attempt.create, attempt.linkID, attempt.update)
	}
	if err != nil {
		slog.Warn("enqueue_commitment_refusal", "error_class", "storage")
	}
}
