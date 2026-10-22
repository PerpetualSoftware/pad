package server

import (
	"errors"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The push dispatcher's privacy gates (TASK-3939 part B1). The enqueue writes
// a row for anyone with a device and checks nothing, so THIS is where a row
// stops being about something its recipient may no longer see. A row that
// fails a gate is skipped with a recorded reason and never reaches the
// gateway. A gate that cannot be evaluated is not a denial: the caller
// unclaims the row, never sends it, and never records a skip.

// Skip reasons the dispatcher records.
const (
	skipUserDisabled   = "user_disabled"
	skipNotMember      = "not_member"
	skipItemGone       = "item_gone"
	skipWorkspaceGone  = "workspace_gone"
	skipNotVisible     = "not_visible"
	skipNoLongerAssign = "no_longer_assigned"
	skipCommentGone    = "comment_gone"
	skipSettingOff     = "setting_off"
	skipAgentOff       = "agent_activity_off"
	skipUnknownKind    = "unknown_kind"
	skipNoDevice       = "no_device"
	skipPredatesDevice = "predates_device"
)

// pushGateCache memoises one tick's lookups. Errors are never cached.
type pushGateCache struct {
	users    map[string]*models.User
	members  map[string]*models.WorkspaceMember
	vis      map[string]watchAccessVisibility
	live     map[string]bool
	subjects map[string]*models.Item
	comments map[string]*models.Comment
	settings map[string]models.NotificationSettings
	devices  map[string][]store.PushDevice
}

func newPushGateCache() *pushGateCache {
	return &pushGateCache{
		users:    map[string]*models.User{},
		members:  map[string]*models.WorkspaceMember{},
		vis:      map[string]watchAccessVisibility{},
		live:     map[string]bool{},
		subjects: map[string]*models.Item{},
		comments: map[string]*models.Comment{},
		settings: map[string]models.NotificationSettings{},
		devices:  map[string][]store.PushDevice{},
	}
}

// notificationAccess is the SHARED gate: may n's recipient still be told
// about n's subject. Order is privacy first, so when several fail the
// recorded reason is the privacy one: user (G4), membership (G1, separate from
// visibility), subject liveness (G3), visibility (G2), then whether the
// reason for the row still holds (G9). reason != "" is a denial; err != nil
// means a gate could not be evaluated. A nil cache is a fresh one.
func (s *Server) notificationAccess(c *pushGateCache, n store.Notification) (item *models.Item, reason string, err error) {
	if c == nil {
		c = newPushGateCache()
	}
	user, ok := c.users[n.UserID]
	if !ok {
		if user, err = s.store.GetUser(n.UserID); err != nil {
			return nil, "", err
		}
		c.users[n.UserID] = user
	}
	// user == nil guards a user deleted between the claim and this gate
	// (user_notifications.user_id cascades on delete, migration 155).
	if user == nil || user.IsDisabled() || user.IsApp() {
		return nil, skipUserDisabled, nil
	}

	mkey := n.WorkspaceID + "/" + n.UserID
	member, ok := c.members[mkey]
	if !ok {
		if member, err = s.store.GetWorkspaceMember(n.WorkspaceID, n.UserID); err != nil {
			return nil, "", err
		}
		c.members[mkey] = member
	}
	if member == nil {
		return nil, skipNotMember, nil
	}

	alive, ok := c.live[n.WorkspaceID]
	if !ok {
		ws, werr := s.store.GetWorkspaceByID(n.WorkspaceID)
		if werr != nil {
			return nil, "", werr
		}
		alive = ws != nil
		c.live[n.WorkspaceID] = alive
	}
	if !alive {
		return nil, skipWorkspaceGone, nil
	}

	item, ok = c.subjects[n.ItemID]
	if !ok {
		item, err = s.store.NotificationSubject(n.ItemID)
		if errors.Is(err, store.ErrNotificationSubjectGone) {
			item, err = nil, nil
		}
		if err != nil {
			return nil, "", err
		}
		c.subjects[n.ItemID] = item
	}
	if item == nil || item.WorkspaceID != n.WorkspaceID {
		return nil, skipItemGone, nil
	}

	vis, ok := c.vis[mkey]
	if !ok {
		// bearerAuth=true: a platform admin's cookie bypass is a property of a
		// live browser connection, not of a push to a phone.
		if vis, _, err = s.resolveWatchAccessVisibility(true, user, n.WorkspaceID); err != nil {
			return nil, "", err
		}
		c.vis[mkey] = vis
	}
	if !vis.allows(item.CollectionID, item.ID) {
		return nil, skipNotVisible, nil
	}

	switch n.Kind {
	case notify.KindAssigned:
		if item.AssignedUserID == nil || *item.AssignedUserID != n.UserID {
			return nil, skipNoLongerAssign, nil
		}
	case notify.KindComment, notify.KindReply, notify.KindMention:
		cm, ok := c.comments[n.CommentID]
		if !ok {
			if n.CommentID != "" {
				if cm, err = s.store.GetComment(n.CommentID); err != nil {
					return nil, "", err
				}
			}
			c.comments[n.CommentID] = cm
		}
		if cm == nil || cm.Deleted || cm.ItemID != n.ItemID || cm.WorkspaceID != n.WorkspaceID {
			return nil, skipCommentGone, nil
		}
	}
	return item, "", nil
}

// kindEnabled applies the recipient's settings (G5) and the agent-activity
// switch (G6) to n. A kind the dispatcher has no text for is skipped, not sent.
func kindEnabled(set models.NotificationSettings, n store.Notification) (reason string) {
	var on bool
	switch n.Kind {
	case notify.KindAssigned:
		on = set.NotifyAssigned
	case notify.KindComment:
		on = set.NotifyComments
	case notify.KindReply:
		on = set.NotifyReplies
	case notify.KindReminder:
		on = set.NotifyReminders
	case notify.KindMention:
		on = set.NotifyMentions
	default:
		return skipUnknownKind
	}
	if !set.PushEnabled || !on {
		return skipSettingOff
	}
	if n.ActorIsAgent && !set.IncludeAgentActivity {
		return skipAgentOff
	}
	return ""
}

// deviceGate is G7/G8: the user's live devices (the latest live registration
// owns a shared token, an expired or revoked session is no device), and the
// row must not predate the earliest of them. No live device is a skip too.
func deviceGate(n store.Notification, devs []store.PushDevice) string {
	if len(devs) == 0 {
		return skipNoDevice
	}
	if n.CreatedAt < devs[0].CreatedAt {
		return skipPredatesDevice
	}
	return ""
}
