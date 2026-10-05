package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
)

// App event DTOs (SPEC-6 U10b, DOC-3371 §5 table; TASK-3408).
//
// Built from the outbox row's app_projection block and the few snapshot keys
// the table names, NOTHING else: no live read of the subject, its schema or
// its parent. The full snapshot (the whole models.Item or models.Comment)
// never reaches an app.

// ErrNoAppProjection: the row carries no usable app-projection block (an
// event from before the first install, or a payload this build cannot read).
// The delivery is SKIPPED and counted, never filled in from live state.
var ErrNoAppProjection = errors.New("event has no app projection")

// AppEventSubscribable is the v1 set an app hook may receive (§5): only
// events whose subject is one companion item or comment. Delivery drops any
// other name even if a hook somehow subscribes to it.
var AppEventSubscribable = map[string]bool{
	kernelevents.ItemCreated: true, kernelevents.ItemUpdated: true, kernelevents.ItemStatusChanged: true,
	kernelevents.ItemDeleted: true, kernelevents.ItemRestored: true,
	kernelevents.CommentCreated: true, kernelevents.CommentUpdated: true, kernelevents.CommentDeleted: true,
}

// AppEventEnvelope is the head every app DTO carries.
type AppEventEnvelope struct {
	Event      string `json:"event"`
	ID         string `json:"id"`
	OccurredAt string `json:"occurred_at"`
	// ViaApp is the install whose write caused this event, absent for a
	// person's or agent's write. From a v2 block it is the frozen
	// actor_via_app, on every event (TASK-3411). A v1 block (written before
	// v2, still in the outbox) has no actor: it is set on CREATE events only,
	// from the creator, who is the writer there (lead ruling, day 87).
	ViaApp string `json:"via_app,omitempty"`
}

// AppEventCreator is the frozen creator, as the app sees it.
type AppEventCreator struct {
	UserID  string `json:"user_id,omitempty"`
	Display string `json:"display,omitempty"`
	Kind    string `json:"kind,omitempty"`
	ViaApp  string `json:"via_app,omitempty"`
}

// AppItemEvent: item.created, item.updated, item.status_changed,
// item.restored. A PARTIAL block (the event-time schema or fields could not
// be read) is served ref-only: no title, content or fields.
type AppItemEvent struct {
	AppEventEnvelope
	ItemID       string          `json:"item_id"`
	CollectionID string          `json:"collection_id"`
	Title        *string         `json:"title,omitempty"`
	Content      *string         `json:"content,omitempty"`
	Fields       *map[string]any `json:"fields,omitempty"`
	Creator      AppEventCreator `json:"creator"`
	CreatedAt    string          `json:"created_at,omitempty"`
	UpdatedAt    string          `json:"updated_at,omitempty"`
	Partial      bool            `json:"partial,omitempty"`
}

// AppItemDeletedEvent: item.deleted. Identifiers only.
type AppItemDeletedEvent struct {
	AppEventEnvelope
	ItemID       string `json:"item_id"`
	CollectionID string `json:"collection_id"`
}

// AppCommentEvent: comment.created, comment.updated.
type AppCommentEvent struct {
	AppEventEnvelope
	CommentID       string          `json:"comment_id"`
	ItemID          string          `json:"item_id"`
	CollectionID    string          `json:"collection_id"`
	ParentCommentID string          `json:"parent_comment_id,omitempty"`
	Body            string          `json:"body"`
	Creator         AppEventCreator `json:"creator"`
	CreatedAt       string          `json:"created_at,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
}

// AppCommentDeletedEvent: comment.deleted. Identifiers only.
type AppCommentDeletedEvent struct {
	AppEventEnvelope
	CommentID       string `json:"comment_id"`
	ItemID          string `json:"item_id"`
	CollectionID    string `json:"collection_id"`
	ParentCommentID string `json:"parent_comment_id,omitempty"`
}

// BuildAppEventDTO builds the app body for one outbox event and returns it
// with the companion collection its visibility is decided by. It reads the
// stored payload only. ErrNoAppProjection when the block is missing.
func BuildAppEventDTO(event, eventID, occurredAt string, payload []byte) ([]byte, string, error) {
	if !AppEventSubscribable[event] {
		return nil, "", fmt.Errorf("%w: %s is not an app event", ErrNoAppProjection, event)
	}
	env := AppEventEnvelope{Event: event, ID: eventID, OccurredAt: occurredAt}
	creator := func(c appProjectionCreator) AppEventCreator {
		return AppEventCreator{UserID: c.UserID, Display: c.Display, Kind: c.Kind, ViaApp: c.ViaApp}
	}
	switch event {
	case kernelevents.ItemCreated, kernelevents.ItemUpdated, kernelevents.ItemStatusChanged, kernelevents.ItemRestored, kernelevents.ItemDeleted:
		var p struct {
			ID            string             `json:"id"`
			Title         string             `json:"title"`
			Content       string             `json:"content"`
			CreatedAt     string             `json:"created_at"`
			UpdatedAt     string             `json:"updated_at"`
			AppProjection *itemAppProjection `json:"app_projection"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNoAppProjection, err)
		}
		b := p.AppProjection
		if b == nil || b.CollectionID == "" || p.ID == "" {
			return nil, "", ErrNoAppProjection
		}
		if event == kernelevents.ItemDeleted {
			env.ViaApp = envelopeViaApp(b.V, b.ActorViaApp, false, "")
			out, err := json.Marshal(AppItemDeletedEvent{AppEventEnvelope: env, ItemID: p.ID, CollectionID: b.CollectionID})
			return out, b.CollectionID, err
		}
		env.ViaApp = envelopeViaApp(b.V, b.ActorViaApp, event == kernelevents.ItemCreated, b.Creator.ViaApp)
		dto := AppItemEvent{AppEventEnvelope: env, ItemID: p.ID, CollectionID: b.CollectionID, Creator: creator(b.Creator),
			CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
		if b.Partial || b.Fields == nil {
			dto.Partial = true
		} else {
			dto.Title, dto.Content, dto.Fields = &p.Title, &p.Content, b.Fields
		}
		out, err := json.Marshal(dto)
		return out, b.CollectionID, err

	case kernelevents.CommentCreated, kernelevents.CommentUpdated:
		var p struct {
			ID            string                `json:"id"`
			Body          string                `json:"body"`
			CreatedAt     string                `json:"created_at"`
			UpdatedAt     string                `json:"updated_at"`
			AppProjection *commentAppProjection `json:"app_projection"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNoAppProjection, err)
		}
		b := p.AppProjection
		if b == nil || b.CollectionID == "" || b.ItemID == "" || p.ID == "" {
			return nil, "", ErrNoAppProjection
		}
		env.ViaApp = envelopeViaApp(b.V, b.ActorViaApp, event == kernelevents.CommentCreated, b.Creator.ViaApp)
		out, err := json.Marshal(AppCommentEvent{AppEventEnvelope: env, CommentID: p.ID, ItemID: b.ItemID, CollectionID: b.CollectionID,
			ParentCommentID: b.ParentCommentID, Body: p.Body, Creator: creator(b.Creator), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
		return out, b.CollectionID, err

	default: // comment.deleted
		var p struct {
			ID            string                `json:"id"`
			AppProjection *commentAppProjection `json:"app_projection"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrNoAppProjection, err)
		}
		b := p.AppProjection
		if b == nil || b.CollectionID == "" || b.ItemID == "" || p.ID == "" {
			return nil, "", ErrNoAppProjection
		}
		env.ViaApp = envelopeViaApp(b.V, b.ActorViaApp, false, "")
		out, err := json.Marshal(AppCommentDeletedEvent{AppEventEnvelope: env, CommentID: p.ID, ItemID: b.ItemID,
			CollectionID: b.CollectionID, ParentCommentID: b.ParentCommentID})
		return out, b.CollectionID, err
	}
}

// envelopeViaApp is the envelope's via_app: the frozen actor from a v2 block;
// from a v1 block, the creator's install on a create event only.
func envelopeViaApp(v int, actor string, isCreate bool, creator string) string {
	if v >= 2 {
		return actor
	}
	if isCreate {
		return creator
	}
	return ""
}
