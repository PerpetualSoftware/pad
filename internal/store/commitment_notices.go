package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// commitmentNoticeRecipientTx resolves the subject and its current recipient.
// Visibility is checked on the transaction, without any platform admin bypass.
func (s *Store) commitmentNoticeRecipientTx(tx *sql.Tx, workspaceID, itemID string) (*models.Item, string, error) {
	item := &models.Item{ID: itemID, WorkspaceID: workspaceID}
	var recipient string
	err := tx.QueryRow(s.q(`SELECT i.collection_id,COALESCE(NULLIF(i.assigned_user_id,''),w.owner_id,'') FROM items i JOIN workspaces w ON w.id=i.workspace_id WHERE i.id=? AND i.workspace_id=? AND i.deleted_at IS NULL AND w.deleted_at IS NULL`), itemID, workspaceID).Scan(&item.CollectionID, &recipient)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if recipient == "" {
		return item, "", nil
	}
	user, err := s.GetUserQ(tx, recipient)
	if err != nil {
		return item, "", err
	}
	if user == nil || user.IsDisabled() || user.IsApp() {
		return item, "", nil
	}
	visible, err := s.notificationRecipientVisibleTx(tx, item, recipient)
	if err != nil || !visible {
		return item, "", err
	}
	return item, recipient, nil
}

func (s *Store) enqueueCommitmentNoticeTx(tx *sql.Tx, workspaceID, itemID, kind, key string) error {
	_, recipient, err := s.commitmentNoticeRecipientTx(tx, workspaceID, itemID)
	if err != nil || recipient == "" {
		return err
	}
	return s.enqueueNotificationsTx(tx, []notifyFact{{UserID: recipient, WorkspaceID: workspaceID, ItemID: itemID, Kind: kind, DedupeKey: key}})
}

// enqueueLinkedMoveNoticesTx groups a committed move by recipient. Its subject
// is one affected item visible to that recipient, with no cross-item payload.
func (s *Store) enqueueLinkedMoveNoticesTx(tx *sql.Tx, p *linkedPlan, rootChange, actor string) error {
	type notice struct {
		item, workspace string
		past            bool
	}
	recipients := map[string]notice{}
	affected := map[string]bool{}
	for _, ch := range p.impact.Changes {
		affected[ch.Node.Schedule] = true
	}
	for _, id := range keysOf(affected) {
		sc, err := s.getScheduleByIDQ(tx, id)
		if err != nil {
			return err
		}
		if sc == nil {
			continue
		}
		_, recipient, err := s.commitmentNoticeRecipientTx(tx, sc.WorkspaceID, sc.ItemID)
		if err != nil {
			return err
		}
		if recipient == "" || recipient == actor {
			continue
		}
		n, err := s.linkNodeTx(tx, sc)
		if err != nil {
			return err
		}
		past := n != nil && !n.dates[schedule.RoleDeadline].IsZero() && n.dates[schedule.RoleTarget].After(n.dates[schedule.RoleDeadline])
		previous, ok := recipients[recipient]
		if !ok || past && !previous.past {
			recipients[recipient] = notice{sc.ItemID, sc.WorkspaceID, past}
		}
	}
	var users []string
	for user := range recipients {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		n := recipients[user]
		kind := "commitment_moved"
		if n.past {
			kind = "commitment_moved_past_deadline"
		}
		if err := s.enqueueNotificationsTx(tx, []notifyFact{{UserID: user, WorkspaceID: n.workspace, ItemID: n.item, Kind: kind, DedupeKey: "commitment-move:" + rootChange}}); err != nil {
			return err
		}
	}
	return nil
}

// RefusedCommitmentMove is the private, canonical identity of an attempted
// operation, never an error or a transport value. It stores no pending mutation.
type RefusedCommitmentMove struct {
	operation, actor, workspace, root, occurrence, role, to, revision string
	offset                                                            models.ScheduleLinkOffset
	policy                                                            string
}

// EnqueueLinkedDeadlineApprovalNotice re-plans a refused move after its caller
// has rolled back. Fresh permissions and the graph decide whether the same
// authority refusal still holds. The input access maps and client revision are
// never used as evidence, and no proposed mutation is stored.
func (s *Store) EnqueueLinkedDeadlineApprovalNotice(workspaceID string, in models.LinkedMoveInput) error {
	if in.Preview {
		return nil
	}
	role := linkRole(in.Role)
	to, err := time.Parse(time.RFC3339, in.To)
	if role == "" || err != nil {
		return nil
	}
	if in.ActorUserID != "" {
		in.Actor = in.ActorUserID
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return err
	}

	user, err := s.GetUserQ(tx, in.Actor)
	if err != nil {
		return err
	}
	if user == nil || user.IsDisabled() || user.IsApp() {
		return nil
	}
	rows, err := tx.Query(s.q(`SELECT id,schema,settings FROM collections WHERE workspace_id=?`), workspaceID)
	if err != nil {
		return err
	}
	filters := scanCollectionDoneFilters(rows)
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	done, args := s.buildChildrenDoneExpr(filters, "i")
	c, err := s.pinCommitmentTx(tx, workspaceID, "NOT "+done, args, false, in.ItemID)
	if errors.Is(err, ErrScheduleItemGone) || errors.Is(err, ErrScheduleNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if matches, err := s.commitmentAssigneeMatchesTx(tx, in.ItemID, in.ExpectedAssignee); err != nil {
		return err
	} else if !matches {
		return nil
	}
	p, err := s.planLinkedTx(tx, c, workspaceID, in.ItemID, role, to.UTC().Truncate(time.Second), true)
	if err != nil {
		var refused *LinkRefusedError
		var field *ScheduleFieldError
		if errors.As(err, &refused) || errors.As(err, &field) {
			return nil
		}
		return err
	}
	access, err := s.refusalAccessTx(tx, workspaceID, in.Actor, p, in.CommitmentAccessLoader)
	if err != nil {
		return err
	}

	if !access.Editable[in.ItemID] {
		return nil
	}
	_, _, err = s.settleLinkedPlan(p, budget, linkedOptions{actor: in.Actor, reason: strings.TrimSpace(in.Reason), access: access})
	if !errors.Is(err, ErrDeadlineApprovalRequired) {
		return nil
	}
	attempt := RefusedCommitmentMove{operation: "move_date", actor: in.Actor, workspace: workspaceID, root: in.ItemID, occurrence: p.nodes[p.root].key, role: linkRoleName(role), to: to.UTC().Truncate(time.Second).Format(time.RFC3339), revision: p.revision}
	if p.incoming != nil {
		attempt.offset = p.incomingOffset
		attempt.policy = p.incoming.CancelPolicy
	}
	return s.enqueueRefusedPlanTx(tx, workspaceID, p, access, attempt)
}

func (s *Store) refusalAccessTx(tx *sql.Tx, workspaceID, actor string, p *linkedPlan, loader CommitmentAccessLoader) (models.CommitmentAccess, error) {
	if loader != nil {
		return loader(tx)
	}
	access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	for _, node := range p.nodes {
		var collection string
		if err := tx.QueryRow(s.q(`SELECT collection_id FROM items WHERE id=?`), node.sc.ItemID).Scan(&collection); err != nil {
			return models.CommitmentAccess{}, err
		}
		perm, err := s.resolveUserPermissionQ(tx, workspaceID, actor, node.sc.ItemID, collection)
		if err != nil {
			return models.CommitmentAccess{}, err
		}
		access.Readable[node.sc.ItemID] = perm != ""
		access.Editable[node.sc.ItemID] = perm == "owner" || perm == "editor" || perm == "edit" || perm == "admin"
	}
	return access, nil
}

func (s *Store) enqueueRefusedPlanTx(tx *sql.Tx, workspaceID string, p *linkedPlan, access models.CommitmentAccess, attempt RefusedCommitmentMove) error {
	canonical, err := json.Marshal(struct {
		Operation, Actor, Workspace, Root, Occurrence, Role, To, Revision string
		Offset                                                            models.ScheduleLinkOffset
		Policy                                                            string
	}{attempt.operation, attempt.actor, attempt.workspace, attempt.root, attempt.occurrence, attempt.role, attempt.to, attempt.revision, attempt.offset, attempt.policy})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	key := "commitment-approval:" + hex.EncodeToString(digest[:])
	for _, ch := range p.impact.HardDeadlines {
		item := p.c.itemOf(ch.Node.Schedule)
		if access.Editable[item] {
			continue
		}
		if err := s.enqueueCommitmentNoticeTx(tx, workspaceID, item, "deadline_approval_requested", key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EnqueueLinkedWriteDeadlineApprovalNotice recomputes the actual link operation
// after rollback. A synthetic direct move would miss offset replacement and
// the link graph revision, so this uses the same link write planner.
func (s *Store) EnqueueLinkedWriteDeadlineApprovalNotice(workspaceID string, create *models.ScheduleLinkCreate, linkID string, update *models.ScheduleLinkUpdate) error {
	if (create == nil) == (update == nil) {
		return nil
	}
	var opts models.LinkWriteOptions
	var parentID, childID string
	var err error
	if create != nil {
		opts = create.Move
		parentID, childID = create.ParentItemID, create.ChildItemID
	} else {
		opts = update.Move
		if update.Offset == nil {
			return nil
		}
		parentID, childID, err = s.linkEndsOf(workspaceID, linkID)
		if errors.Is(err, ErrScheduleLinkNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if opts.Preview || opts.CommitmentAccessLoader == nil {
		return nil
	}
	actor := opts.ActorUserID
	if actor == "" {
		actor = opts.Actor
	}
	budget := s.newLinkBudget()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.setTxTimeoutsCtx(budget.ctx, tx, linkTxBudget, linkTxBudget); err != nil {
		return err
	}

	user, err := s.GetUserQ(tx, actor)
	if err != nil {
		return err
	}
	if user == nil || user.IsDisabled() || user.IsApp() {
		return nil
	}
	rows, err := tx.Query(s.q(`SELECT id,schema,settings FROM collections WHERE workspace_id=?`), workspaceID)
	if err != nil {
		return err
	}
	filters := scanCollectionDoneFilters(rows)
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	done, args := s.buildChildrenDoneExpr(filters, "i")
	c, err := s.pinCommitmentTx(tx, workspaceID, "NOT "+done, args, true, parentID, childID)
	if errors.Is(err, ErrScheduleItemGone) || errors.Is(err, ErrScheduleNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var parent, child *models.ItemSchedule
	for _, sc := range c.scheds {
		if sc.ItemID == parentID {
			parent = sc
		}
		if sc.ItemID == childID {
			child = sc
		}
	}
	if parent == nil || child == nil {
		return nil
	}
	if create != nil {
		for _, end := range []struct {
			sc   *models.ItemSchedule
			role string
		}{{parent, create.ParentRole}, {child, create.ChildRole}} {
			roles, err := s.scheduleRolesOf(tx, end.sc)
			if err != nil {
				return err
			}
			if !roleConfigured(roles, end.role) || (end.role == "deadline" && roles.Origin != "native") {
				return nil
			}
		}
	}
	l := linkRow{}
	operation := "create_link"
	if create != nil {
		policy := create.CancelPolicy
		if policy == "" {
			policy = models.LinkWaitingOn
		}
		off, err := offsetOf(create.Offset)
		if err != nil {
			return nil
		}
		l = linkRow{ParentSchedule: parent.ID, ParentRole: create.ParentRole, ChildSchedule: child.ID, ChildRole: create.ChildRole, Offset: storedOffset(off), CancelPolicy: policy}
		if len(c.links)+1 > maxLinkComponent {
			return nil
		}
	} else {
		operation = "update_link"
		found := false
		for _, cur := range c.links {
			if cur.ID == linkID {
				l = cur
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		off, err := offsetOf(*update.Offset)
		if err != nil {
			return nil
		}
		l.Offset = storedOffset(off)
		if update.CancelPolicy != nil {
			l.CancelPolicy = *update.CancelPolicy
		}
		c.explicitLink = linkID
	}
	g, err := c.structuralGraph(l.ID)
	if err != nil {
		return err
	}
	edge, err := l.edge()
	if err != nil {
		return nil
	}
	if err = schedule.CheckLink(g, edge, schedule.LinkLimits{}); err != nil {
		return nil
	}
	// A supplied HTTP loader preserves token and application boundaries too.
	access, err := opts.CommitmentAccessLoader(tx)
	if err != nil {
		return err
	}
	if !readableLinkEnds(access, parentID, childID) || !access.Editable[parentID] || !access.Editable[childID] {
		return nil
	}
	opts.Revision = ""
	_, p, _, err := s.linkWritePlanTx(tx, workspaceID, c, l, parent, budget, opts, access)
	if !errors.Is(err, ErrDeadlineApprovalRequired) {
		return nil
	}
	attempt := RefusedCommitmentMove{operation: operation, actor: actor, workspace: workspaceID, root: parentID, occurrence: p.nodes[p.root].key, role: l.ParentRole, revision: p.revision, offset: l.Offset, policy: l.CancelPolicy}
	return s.enqueueRefusedPlanTx(tx, workspaceID, p, access, attempt)
}
