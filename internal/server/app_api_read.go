package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The app API's read handlers and response DTOs (SPEC-6 §4, U6a). Each
// handler authorizes through the shared helpers under the ceiling and encodes
// a FIXED DTO: anything a DTO does not name is never sent, whatever the store
// row carries. The DTO census test pins every key, nested ones included.

// AppSchemaField is one field of AppCollection's schema projection: declared
// non-relation fields only. Never a target collection; a default only for a
// scalar, non-reference type.
type AppSchemaField struct {
	Key      string   `json:"key"`
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required"`
	Default  any      `json:"default,omitempty"`
}

// AppCollectionSchema is the projection of a collection's schema.
type AppCollectionSchema struct {
	Fields []AppSchemaField `json:"fields"`
}

// AppCollection never carries settings, traits, counts, relation fields or
// any target collection slug.
type AppCollection struct {
	Slug     string              `json:"slug"`
	Name     string              `json:"name"`
	Icon     string              `json:"icon"`
	IsSystem bool                `json:"is_system"`
	Schema   AppCollectionSchema `json:"schema"`
}

// AppItem never carries slug, ref, seq, parent, lease, decisions, moved_to,
// relation_targets, assignee, role, github_pr, implementation notes or the
// decision log.
type AppItem struct {
	ID               string         `json:"id"`
	Collection       string         `json:"collection"`
	Title            string         `json:"title"`
	Content          string         `json:"content"`
	Fields           map[string]any `json:"fields"`
	ETag             string         `json:"etag"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	CreatedByDisplay string         `json:"created_by_display"`
	ViaApp           string         `json:"via_app"`
}

// AppComment never carries reactions or user_id.
type AppComment struct {
	ID              string    `json:"id"`
	ItemID          string    `json:"item_id"`
	Body            string    `json:"body"`
	AuthorDisplay   string    `json:"author_display"`
	AuthorKind      string    `json:"author_kind"`
	ParentCommentID *string   `json:"parent_comment_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Edited          bool      `json:"edited"`
	Deleted         bool      `json:"deleted"`
}

// AppMe never carries the email, platform role, TOTP or billing.
type AppMe struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	IsApp       bool   `json:"is_app"`
}

// scalarFieldTypes are the types whose default an app may see.
var scalarFieldTypes = map[string]bool{"text": true, "url": true, "number": true, "checkbox": true, "date": true, "select": true, "multi_select": true}

func appSchemaProjection(c *models.Collection) AppCollectionSchema {
	out := AppCollectionSchema{Fields: []AppSchemaField{}}
	var schema models.CollectionSchema
	if c.Schema == "" || models.UnmarshalItemFieldSchema([]byte(c.Schema), &schema) != nil {
		return out
	}
	for _, f := range schema.Fields {
		if f.Type == "relation" || f.IsMultiRelation() {
			continue
		}
		af := AppSchemaField{Key: f.Key, Type: f.Type, Label: f.Label, Options: f.Options, Required: f.Required}
		if scalarFieldTypes[f.Type] {
			af.Default = f.Default
		}
		out.Fields = append(out.Fields, af)
	}
	return out
}

// appFieldProjection keeps only the item's values for the collection's
// declared non-relation keys.
func appFieldProjection(fieldsJSON string, c *models.Collection) map[string]any {
	out := map[string]any{}
	allowed := map[string]bool{}
	for _, f := range appSchemaProjection(c).Fields {
		allowed[f.Key] = true
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(fieldsJSON)))
	dec.UseNumber()
	var raw map[string]any
	if fieldsJSON == "" || dec.Decode(&raw) != nil {
		return out
	}
	for k, v := range raw {
		if allowed[k] {
			out[k] = v
		}
	}
	return out
}

func writeAppJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAppNotFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, "not_found", what+" not found")
}

// appVisibleCollection resolves a collection by EXACT slug in the token's
// workspace, inside the ceiling and the actor's visibility, or reports false
// after answering the same 404 a missing collection gets.
func (s *Server) appVisibleCollection(w http.ResponseWriter, r *http.Request, slug string) (*models.Collection, bool) {
	ac := appContextFrom(r)
	c, err := s.store.GetCollectionBySlug(ac.WorkspaceID, slug)
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	if c == nil || c.DeletedAt != nil || c.Slug != slug {
		writeAppNotFound(w, "Collection")
		return nil, false
	}
	ids, err := s.visibleCollectionIDs(r, ac.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	if !isCollectionVisible(c.ID, ids) || !appCeilingAllows(r, c.ID) {
		writeAppNotFound(w, "Collection")
		return nil, false
	}
	id := c.ID
	appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckCollection(r2, id) })
	return c, true
}

var errAppRecheck = errors.New("app authorization no longer holds")

// appRecheckCollection replays a collection's authorization against the
// store as it stands: live, in the workspace, visible to the actor, inside
// the ceiling.
func (s *Server) appRecheckCollection(r *http.Request, collectionID string) error {
	if memo, ok := r.Context().Value(appRecheckMemoKey{}).(*appRecheckMemo); ok {
		if err, seen := memo.collections[collectionID]; seen {
			return err
		}
		err := s.appRecheckCollectionUncached(r, collectionID)
		memo.collections[collectionID] = err
		return err
	}
	return s.appRecheckCollectionUncached(r, collectionID)
}

func (s *Server) appRecheckCollectionUncached(r *http.Request, collectionID string) error {
	ac := appContextFrom(r)
	c, err := s.store.GetCollection(collectionID)
	if err != nil {
		return appFault(err)
	}
	if c == nil || c.DeletedAt != nil || c.WorkspaceID != ac.WorkspaceID {
		return errAppRecheck
	}
	ids, err := s.visibleCollectionIDs(r, ac.WorkspaceID)
	if err != nil {
		return appFault(err)
	}
	if !isCollectionVisible(c.ID, ids) || !appCeilingAllows(r, c.ID) {
		return errAppRecheck
	}
	return nil
}

// appRecheckItem replays an item's authorization: live, in the workspace and
// collection it was read from, inside the ceiling, visible to the actor.
func (s *Server) appRecheckItem(r *http.Request, itemID, collectionID string) error {
	ac := appContextFrom(r)
	it, err := s.store.GetItem(itemID)
	if err != nil {
		return appFault(err)
	}
	if it == nil || it.DeletedAt != nil || it.WorkspaceID != ac.WorkspaceID || it.CollectionID != collectionID {
		return errAppRecheck
	}
	if !appCeilingAllows(r, it.CollectionID) {
		return errAppRecheck
	}
	ok, err := s.checkItemVisible(ac.WorkspaceID, it, currentUser(r), workspaceRole(r), isBearerAuth(r))
	if err != nil {
		return appFault(err)
	}
	if !ok {
		return errAppRecheck
	}
	return s.appRecheckCollection(r, collectionID)
}

func (s *Server) appCollectionDTO(c *models.Collection) AppCollection {
	return AppCollection{Slug: c.Slug, Name: c.Name, Icon: c.Icon, IsSystem: c.IsSystem, Schema: appSchemaProjection(c)}
}

func (s *Server) appListCollections(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	all, err := s.store.ListCollections(ac.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	ids, err := s.visibleCollectionIDs(r, ac.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := []AppCollection{}
	for i := range all {
		c := &all[i]
		if c.DeletedAt != nil || !isCollectionVisible(c.ID, ids) || !appCeilingAllows(r, c.ID) {
			continue
		}
		out = append(out, s.appCollectionDTO(c))
		id := c.ID
		appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckCollection(r2, id) })
	}
	writeAppJSON(w, http.StatusOK, map[string]any{"collections": out})
}

func (s *Server) appGetCollection(w http.ResponseWriter, r *http.Request) {
	c, ok := s.appVisibleCollection(w, r, chi.URLParam(r, "collSlug"))
	if !ok {
		return
	}
	writeAppJSON(w, http.StatusOK, s.appCollectionDTO(c))
}

// appItemDTOs builds the item DTOs, etag and app metadata included.
func (s *Server) appItemDTOs(r *http.Request, items []models.Item, c *models.Collection) ([]AppItem, error) {
	ac := appContextFrom(r)
	as, err := s.appStore()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	meta, err := s.store.ItemsAppReadMeta(ids)
	if err != nil {
		return nil, err
	}
	spec := ac.appFenceSpec()
	out := make([]AppItem, 0, len(items))
	for i := range items {
		it := &items[i]
		etag, err := as.ETag(spec, it)
		if err != nil {
			return nil, err
		}
		m := meta[it.ID]
		out = append(out, AppItem{
			ID: it.ID, Collection: c.Slug, Title: it.Title, Content: it.Content,
			Fields: appFieldProjection(it.Fields, c), ETag: etag,
			CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
			CreatedByDisplay: m.CreatorDisplay, ViaApp: m.ViaApp,
		})
	}
	return out, nil
}

// appListLimit bounds a list: default 50, at most 200.
func appListLimit(r *http.Request) (int, int, bool) {
	limit, offset := 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

func (s *Server) appListItems(w http.ResponseWriter, r *http.Request) {
	c, ok := s.appVisibleCollection(w, r, chi.URLParam(r, "collSlug"))
	if !ok {
		return
	}
	limit, offset, ok := appListLimit(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be 1-200 and offset >= 0")
		return
	}
	ac := appContextFrom(r)
	// One row past the window decides has_more exactly. The window is over
	// rows the actor may see, filtered in SQL as the human list filters a
	// restricted member's or guest's item grants (guestResourceFilter): a
	// person acting through the app (TASK-3399) may hold item grants only,
	// and a window over hidden rows would let has_more and next_offset count
	// them (codex U5b-2 r2). The per-item check below stays, as a second
	// layer.
	params := models.ItemListParams{ScopeCollectionID: c.ID, CollectionIDs: []string{c.ID}, Limit: limit + 1, Offset: offset}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, ac.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if len(grantedItemIDs) > 0 {
		params.CollectionIDs = fullCollIDs
		if params.CollectionIDs == nil {
			params.CollectionIDs = []string{}
		}
		params.ItemIDs = grantedItemIDs
	}
	items, err := s.store.ListItems(ac.WorkspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	// Item-level visibility, as every human list applies it.
	visible := items[:0]
	for i := range items {
		ok, err := s.checkItemVisible(ac.WorkspaceID, &items[i], currentUser(r), workspaceRole(r), isBearerAuth(r))
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if ok && items[i].CollectionID == c.ID {
			visible = append(visible, items[i])
			id, coll := items[i].ID, c.ID
			appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckItem(r2, id, coll) })
		}
	}
	dtos, err := s.appItemDTOs(r, visible, c)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeAppJSON(w, http.StatusOK, map[string]any{"items": dtos, "has_more": hasMore, "next_offset": offset + limit})
}

// appVisibleItem resolves an item by ID in the token's workspace, inside the
// ceiling and the actor's visibility, or answers the same 404 a missing item
// gets. Apps never address items by slug or ref.
func (s *Server) appVisibleItem(w http.ResponseWriter, r *http.Request) (*models.Item, *models.Collection, bool) {
	ac := appContextFrom(r)
	it, err := s.store.GetItem(chi.URLParam(r, "itemID"))
	if err != nil {
		writeInternalError(w, err)
		return nil, nil, false
	}
	if it == nil || it.WorkspaceID != ac.WorkspaceID || it.DeletedAt != nil {
		writeAppNotFound(w, "Item")
		return nil, nil, false
	}
	if !s.requireItemVisible(w, r, ac.WorkspaceID, it) {
		return nil, nil, false
	}
	c, err := s.store.GetCollection(it.CollectionID)
	if err != nil {
		writeInternalError(w, err)
		return nil, nil, false
	}
	if c == nil || c.DeletedAt != nil {
		writeAppNotFound(w, "Item")
		return nil, nil, false
	}
	id, coll := it.ID, it.CollectionID
	appAddRecheck(r, func(r2 *http.Request) error { return s.appRecheckItem(r2, id, coll) })
	return it, c, true
}

func (s *Server) appGetItem(w http.ResponseWriter, r *http.Request) {
	it, c, ok := s.appVisibleItem(w, r)
	if !ok {
		return
	}
	dtos, err := s.appItemDTOs(r, []models.Item{*it}, c)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeAppJSON(w, http.StatusOK, dtos[0])
}

func (s *Server) appListComments(w http.ResponseWriter, r *http.Request) {
	it, _, ok := s.appVisibleItem(w, r)
	if !ok {
		return
	}
	comments, err := s.store.ListComments(it.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	userIDs := []string{}
	onItem := map[string]bool{}
	for _, c := range comments {
		onItem[c.ID] = true
		if c.UserID != "" {
			userIDs = append(userIDs, c.UserID)
		}
	}
	kinds, err := s.store.UserKinds(userIDs)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := make([]AppComment, 0, len(comments))
	for _, c := range comments {
		kind := c.CreatedBy
		if kinds[c.UserID] == models.UserKindApp {
			kind = "app"
		} else if kind != "agent" {
			kind = "user"
		}
		dto := AppComment{ID: c.ID, ItemID: c.ItemID, Body: c.Body, AuthorDisplay: c.Author, AuthorKind: kind,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Edited: c.IsEdited(), Deleted: c.Deleted}
		// A parent that is not a comment on this same item projects to null:
		// legacy rows can point across items (the FK does not prevent it).
		if c.ParentID != "" && onItem[c.ParentID] {
			p := c.ParentID
			dto.ParentCommentID = &p
		}
		out = append(out, dto)
	}
	writeAppJSON(w, http.StatusOK, map[string]any{"comments": out})
}

// AppInstallMe is the unscoped GET /api/app/v1/me (BUG-3416): the token's
// binding. UserID and DisplayName are the actor: the install's bot for a
// service token, the person for a delegated one.
type AppInstallMe struct {
	InstallID     string `json:"install_id"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceSlug string `json:"workspace_slug"`
	AuthKind      string `json:"auth_kind"`
	Access        string `json:"access"`
	UserID        string `json:"user_id"`
	DisplayName   string `json:"display_name"`
	IsApp         bool   `json:"is_app"`
}

func (s *Server) appInstallMe(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	writeAppJSON(w, http.StatusOK, AppInstallMe{
		InstallID: ac.InstallID, WorkspaceID: ac.WorkspaceID, WorkspaceSlug: ac.WorkspaceSlug,
		AuthKind: ac.AuthKind, Access: ac.Access,
		UserID: ac.Actor.ID, DisplayName: ac.Actor.Name, IsApp: ac.Actor.IsApp(),
	})
}

func (s *Server) appMe(w http.ResponseWriter, r *http.Request) {
	ac := appContextFrom(r)
	writeAppJSON(w, http.StatusOK, AppMe{UserID: ac.Actor.ID, DisplayName: ac.Actor.Name, Role: workspaceRole(r), IsApp: ac.Actor.IsApp()})
}
