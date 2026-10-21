package notify

import "fmt"

// Notification kinds as the gateway carries them in data.kind. The five
// per-event kinds are user_notifications.kind; KindSummary is the one push
// that stands for several.
const (
	KindAssigned = "assigned"
	KindComment  = "comment"
	KindReply    = "reply"
	KindReminder = "reminder"
	KindMention  = "mention"
	KindSummary  = "summary"
)

// Title is the fixed title of every push.
const Title = "Pad"

// Data is the gateway message's data block. Everything the app needs to find
// the notification is an id: it fetches the content itself, authenticated.
type Data struct {
	NID   string `json:"nid"`
	CID   string `json:"cid"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Text returns the fixed body for kind. It is content-free by rule: no item
// title, comment text, workspace name or actor name ever reaches a push
// provider. count only matters for the summary. ok is false for an unknown
// kind, which a caller must treat as a bug and not send.
func Text(kind string, count int) (body string, ok bool) {
	switch kind {
	case KindAssigned:
		return "You were assigned an item", true
	case KindComment:
		return "New comment on your item", true
	case KindReply:
		return "New reply to your comment", true
	case KindReminder:
		return "Reminder due", true
	case KindMention:
		return "You were mentioned", true
	case KindSummary:
		return fmt.Sprintf("You have %d new notifications", count), true
	}
	return "", false
}

// NewMessage builds the message for one device: the fixed title and body for
// kind, and Data{nid, cid, kind, count}. id is the batch-local message id.
func NewMessage(id, platform, token, kind, nid, cid string, count int) (Message, bool) {
	if count < 1 {
		count = 1
	}
	body, ok := Text(kind, count)
	if !ok {
		return Message{}, false
	}
	return Message{
		ID: id, Platform: platform, Token: token, Title: Title, Body: body,
		Data: Data{NID: nid, CID: cid, Kind: kind, Count: count},
	}, true
}

// BatchID is the id for the i-th message of a request: m0, m1, ... Batch-local
// and short, because the gateway wants 1 to 64 chars of [A-Za-z0-9_-] unique
// within the request, and one device can appear twice in a batch.
func BatchID(i int) string { return fmt.Sprintf("m%d", i) }
