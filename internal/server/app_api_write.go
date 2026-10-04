package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/appstore"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// The app API's write handlers (SPEC-6 §4, U6b). Each is thin: a fixed
// request DTO with unknown fields refused; the target resolved under the
// ceiling; the write rule; the subject's edit check; ONE appstore call, which
// runs in a FencedTx (epoch fence, companion check, created_via_app, etag,
// caps); then the side effects the parity table moves into shared functions
// (SSE, watch notifications); then a fixed response DTO.
//
// Writes are not re-validated after commit (lead ruling R2): see the route
// wrapper in app_api.go.

// Request DTOs: the only fields that exist.
type AppItemCreateRequest struct {
	Title   string         `json:"title"`
	Content string         `json:"content"`
	Fields  map[string]any `json:"fields"`
}

type AppItemUpdateRequest struct {
	FieldsPatch  map[string]any `json:"fields_patch"`
	ExpectedETag string         `json:"expected_etag"`
}

type AppCommentCreateRequest struct {
	Body            string `json:"body"`
	ParentCommentID string `json:"parent_comment_id"`
}

type AppCommentUpdateRequest struct {
	Body string `json:"body"`
}

// decodeAppJSON decodes a request DTO through the ordinary body rules (size
// cap, NUL refusal, repeated members) and refuses any key the DTO does not
// name, so nothing a DTO does not name can be written.
func decodeAppJSON(r *http.Request, v any) error {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decodeJSONBytes(raw, v); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&keys); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	allowed := map[string]bool{}
	tp := reflect.TypeOf(v).Elem()
	for i := 0; i < tp.NumField(); i++ {
		allowed[strings.Split(tp.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for k := range keys {
		if !allowed[k] {
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

// appActor is the FencedActor for this request: the bot as an agent for a
// service token (§4: the actor is decided by token kind; X-Pad-Agent is
// ignored). A delegated person (TASK-3399) will be "user" with their own id.
func appActor(ac *appContext) store.FencedActor {
	return store.FencedActor{Kind: "agent", UserID: ac.Actor.ID, AgentName: ac.Actor.Name}
}

// writeAppStoreError maps an appstore refusal to its response.
func writeAppStoreError(w http.ResponseWriter, err error) {
	switch {
	case appstore.IsInputError(err):
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, store.ErrFenceStale), errors.Is(err, store.ErrFenceClosed):
		writeAppUnauthorized(w)
	case errors.Is(err, store.ErrNotCompanion):
		writeError(w, http.StatusForbidden, "forbidden", "This app may not write here")
	case errors.Is(err, store.ErrAppNotAppItem):
		writeError(w, http.StatusForbidden, "forbidden", "This app did not create this item")
	case errors.Is(err, store.ErrAppETagMismatch):
		writeError(w, http.StatusPreconditionFailed, "etag_mismatch", "The item changed; fetch it again")
	case errors.Is(err, store.ErrAppItemLimit):
		// No counts: the human renderer's totals are workspace-wide.
		writeError(w, http.StatusForbidden, "item_limit_reached", "The workspace has reached its item limit")
	case errors.Is(err, store.ErrAppCommentNotFound):
		writeAppNotFound(w, "Comment")
	case errors.Is(err, store.ErrAppNotCommentAuthor):
		writeError(w, http.StatusForbidden, "forbidden", "Only the comment's author may change it")
	case errors.Is(err, store.ErrCommentDeleted):
		writeError(w, http.StatusConflict, "comment_deleted", "The comment was deleted")
	case errors.Is(err, store.ErrAppAttachmentUnavailable):
		writeError(w, http.StatusUnprocessableEntity, "attachment_unavailable", "An attachment reference is not available")
	case errors.Is(err, store.ErrAppCrossItemThread):
		writeError(w, http.StatusConflict, "cross_item_thread", "The comment's thread leaves this item")
	default:
		writeInternalError(w, err)
	}
}

func (s *Server) appCreateItem(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	c, ok := s.appVisibleCollection(w, r, chi.URLParam(r, "collSlug"))
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, ac.WorkspaceID, "", c.ID) {
		return
	}
	var in AppItemCreateRequest
	if err := decodeAppJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	wr, err := as.CreateItemWrite(r.Context(), ac.appFenceSpec(), c.ID, appstore.AppItemCreate{Title: in.Title, Content: in.Content, Fields: in.Fields}, appActor(ac))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	item := wr.Item
	s.publishItemEventWithName(sseItemCreated, ac.WorkspaceID, item.ID, item.Title, wr.View.CollectionSlug, "agent", ac.Actor.Name, "app", item.Seq)
	writeAppJSON(w, http.StatusCreated, appWrittenItemDTO(wr))
}

// appWrittenItemDTO is a write's response, built only from what the write's
// own transaction read (appstore.ItemWrite). Nothing here touches the
// database: the write has committed, and a later read could fail and report
// a committed write as failed (inviting a duplicate retry), or describe a
// state the write did not produce, such as a schema changed since the
// handler resolved the collection.
func appWrittenItemDTO(wr *appstore.ItemWrite) AppItem {
	it := wr.Item
	c := &models.Collection{Slug: wr.View.CollectionSlug, Schema: wr.View.SchemaJSON}
	return AppItem{
		ID: it.ID, Collection: c.Slug, Title: it.Title, Content: it.Content,
		Fields: appFieldProjection(it.Fields, c), ETag: wr.ETag,
		CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		CreatedByDisplay: wr.View.CreatorDisplay, ViaApp: wr.View.ViaApp,
	}
}

func (s *Server) appUpdateItem(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	before, _, ok := s.appVisibleItem(w, r)
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, ac.WorkspaceID, before.ID, before.CollectionID) {
		return
	}
	var in AppItemUpdateRequest
	if err := decodeAppJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	wr, err := as.UpdateItemWrite(r.Context(), ac.appFenceSpec(), before.ID, appstore.AppItemUpdate{FieldsPatch: in.FieldsPatch, ExpectedETag: in.ExpectedETag}, appActor(ac))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	after := wr.Item
	s.publishItemEventWithName(sseItemUpdated, ac.WorkspaceID, after.ID, after.Title, wr.View.CollectionSlug, "agent", ac.Actor.Name, "app", after.Seq)
	// Watchers hear about an app's status change as about anyone's (lead
	// ruling R1). The signal is the fenced transaction's own, never a
	// comparison with this handler's earlier read: a status another writer
	// changed between that read and the commit is not this write's.
	if after.LastMutation != nil && after.LastMutation.StatusChanged {
		s.publishWatchNotifications(ac.WorkspaceID, after, "agent", ac.Actor.Name)
	}
	writeAppJSON(w, http.StatusOK, appWrittenItemDTO(wr))
}

// appCommentDTO is one comment's DTO, written by the bot.
func appCommentDTO(c *models.Comment) AppComment {
	dto := AppComment{ID: c.ID, ItemID: c.ItemID, Body: c.Body, AuthorDisplay: c.Author, AuthorKind: "app",
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Edited: c.IsEdited(), Deleted: c.Deleted}
	if c.ParentID != "" {
		p := c.ParentID
		dto.ParentCommentID = &p
	}
	return dto
}

func (s *Server) appCreateComment(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	item, c, ok := s.appVisibleItem(w, r)
	if !ok {
		return
	}
	if !s.requireEditPermission(w, r, ac.WorkspaceID, item.ID, item.CollectionID) {
		return
	}
	var in AppCommentCreateRequest
	if err := decodeAppJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// The author is server-set: the app's bot display name (lead ruling R3).
	comment, err := as.CreateComment(r.Context(), ac.appFenceSpec(), item.ID,
		appstore.AppCommentCreate{Body: in.Body, ParentID: in.ParentCommentID, Author: ac.Actor.Name}, appActor(ac))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	s.publishCommentEvent(sseCommentCreated, ac.WorkspaceID, item.ID, comment.ID, item.Title, c.Slug, "agent", "app")
	s.publishWatchNotification(watchevents.Notification{
		WorkspaceID:  ac.WorkspaceID,
		ItemID:       item.ID,
		CollectionID: item.CollectionID,
		ItemRef:      item.Ref,
		Kind:         watchevents.KindComment,
		Actor:        "agent",
		ActorName:    ac.Actor.Name,
		Summary:      truncateForSummary(comment.Body, 120),
	})
	writeAppJSON(w, http.StatusCreated, appCommentDTO(comment))
}

// appCommentOnItem resolves the comment for a PATCH or DELETE: the item
// first, then the comment, which must be on THAT item, else the same 404 a
// missing comment gets.
func (s *Server) appCommentOnItem(w http.ResponseWriter, r *http.Request) (*models.Item, *models.Collection, *models.Comment, bool) {
	ac := appContextFrom(r)
	item, c, ok := s.appVisibleItem(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	comment, err := s.store.GetComment(chi.URLParam(r, "commentID"))
	if err != nil {
		writeInternalError(w, err)
		return nil, nil, nil, false
	}
	if comment == nil || comment.ItemID != item.ID {
		writeAppNotFound(w, "Comment")
		return nil, nil, nil, false
	}
	if !s.requireEditPermission(w, r, ac.WorkspaceID, item.ID, item.CollectionID) {
		return nil, nil, nil, false
	}
	return item, c, comment, true
}

func (s *Server) appUpdateComment(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	item, c, comment, ok := s.appCommentOnItem(w, r)
	if !ok {
		return
	}
	var in AppCommentUpdateRequest
	if err := decodeAppJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Author only, no admin bypass: the appstore checks user AND install.
	updated, err := as.UpdateComment(r.Context(), ac.appFenceSpec(), item.ID, comment.ID, in.Body, appActor(ac))
	if err != nil {
		writeAppStoreError(w, err)
		return
	}
	s.publishCommentEvent(sseCommentUpdated, ac.WorkspaceID, item.ID, updated.ID, item.Title, c.Slug, "agent", "app")
	writeAppJSON(w, http.StatusOK, appCommentDTO(updated))
}

func (s *Server) appDeleteComment(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	item, _, comment, ok := s.appCommentOnItem(w, r)
	if !ok {
		return
	}
	as, err := s.appStore()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Author only; refused if the comment's ancestor chain leaves the item.
	// The human path publishes no SSE on delete, and neither does this one.
	if err := as.DeleteComment(r.Context(), ac.appFenceSpec(), item.ID, comment.ID, appActor(ac)); err != nil {
		writeAppStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
