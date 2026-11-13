package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Every read uses executeRequest: no direct store access or alternate auth path.
func (d *HTTPHandlerDispatcher) dispatchBoard(action string) specialDispatchFn {
	return func(ctx context.Context, input map[string]any, user *models.User) (*CallToolResult, error) {
		key := "board " + action
		workspace, _ := input["workspace"].(string)
		if workspace == "" {
			return noWorkspaceResult(ctx, d.Lister), nil
		}
		collection, _ := input["collection"].(string)
		if collection == "" {
			return validationFailedResult(key, "collection is required", "Pass a collection slug."), nil
		}
		if action == "changes" {
			for _, name := range []string{"since", "access_epoch"} {
				if _, ok := input[name].(string); !ok {
					return validationFailedResult(key, name+" is required and must be a string", "Pass the cursor and access_epoch from the previous response."), nil
				}
			}
		}
		base := "/api/v1/workspaces/" + url.PathEscape(workspace)
		get := func(path string) *CallToolResult {
			res, err := d.executeRequest(ctx, key, user, http.MethodGet, path, nil)
			if err != nil {
				return dispatcherErrorResult(key, "execute request", err)
			}
			return res
		}
		if action == "changes" {
			since := input["since"].(string)
			res := get(base + "/items-changes?" + url.Values{"since": {since}, "limit": {"500"}}.Encode())
			if res.IsError {
				return res, nil
			}
			var page boardDoorPage
			if err := decodeBoardText(res, func(raw []byte) error { return json.Unmarshal(raw, &page) }); err != nil {
				return dispatcherErrorResult(key, "decode changes", err), nil
			}
			upserts := []boardItem{}
			removed, departed := []string{}, []string{}
			parent, _ := input["parent"].(string)
			for _, row := range page.Changes {
				switch {
				case row.MovedOut:
					// A tombstone carries only id and seq, not the collection
					// it left, so it may have been on this board.
					removed = append(removed, row.ID)
				case row.CollectionSlug != collection:
					// A live row elsewhere is either an unrelated item or a
					// full member's move out of this board; the door does not
					// say which. Blind spot: an item the snapshot omitted
					// (truncated) that moves away leaves its column total one
					// too high until the next show (the view re-shows an
					// incomplete board at least every five minutes).
					if !row.Deleted {
						departed = append(departed, row.ID)
					}
				case row.Deleted || !boardParentMatches(row.ParentRef, parent):
					removed = append(removed, row.ID)
				default:
					upserts = append(upserts, projectBoardItem(row.Item))
				}
			}
			return boardResult(map[string]any{"upserts": upserts, "removed_ids": removed, "departed_ids": departed, "cursor": page.Cursor, "access_epoch": page.AccessEpoch, "resync": page.AccessEpoch != input["access_epoch"] || len(page.Changes) == 500}), nil
		}
		col, failure := boardCollection(get(base+"/collections"), collection, key)
		if failure != nil {
			return failure, nil
		}
		var schema models.CollectionSchema
		// The columns are the status field's options: an item-field use, so the
		// schema goes through the reserved-field stripping decoder.
		if err := models.UnmarshalItemFieldSchema([]byte(col.Schema), &schema); err != nil {
			return dispatcherErrorResult(key, "decode schema", err), nil
		}
		var status *models.FieldDef
		for i := range schema.Fields {
			f := &schema.Fields[i]
			if f.Key == "status" && f.Type == "select" {
				status = f
				break
			}
		}
		if status == nil {
			return validationFailedResult(key, "Collection has no status select field", "Choose a collection with a status select field."), nil
		}
		res := get(base + "/items-index?" + url.Values{"collection": {col.Slug}}.Encode())
		if res.IsError {
			return res, nil
		}
		var page boardDoorPage
		if err := decodeBoardText(res, func(raw []byte) error { return json.Unmarshal(raw, &page) }); err != nil {
			return dispatcherErrorResult(key, "decode index", err), nil
		}
		columns := make([]boardColumn, 0, len(status.Options))
		// Terminal and abandoned come from the shared resolvers, so a status
		// field that declares no terminal_options gets the default terminal
		// list and the usual case-insensitive matching, as elsewhere in Pad.
		_, abandonedValues := models.AbandonedValuesForDoneField(schema, models.CollectionSettings{})
		for _, option := range status.Options {
			terminal := models.IsTerminalStatus(option, schema)
			abandoned := false
			if terminal {
				for _, v := range abandonedValues {
					if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(option)) {
						abandoned = true
					}
				}
			}
			columns = append(columns, boardColumn{Status: option, Terminal: terminal, Abandoned: abandoned, Items: []boardItem{}})
		}
		// The door has no parent filter. Filter only the projected snapshot;
		// its cursor and epoch remain opaque and untouched.
		parent, _ := input["parent"].(string)
		sort.SliceStable(page.Items, func(i, j int) bool { return page.Items[i].UpdatedAt.After(page.Items[j].UpdatedAt) })
		open, unplaced := 0, 0
		truncated := false
		for _, it := range page.Items {
			if !boardParentMatches(it.ParentRef, parent) {
				continue
			}
			item := projectBoardItem(it)
			placed := false
			for i := range columns {
				c := &columns[i]
				if c.Status != item.Status {
					continue
				}
				placed = true
				c.Total++
				if (c.Terminal && len(c.Items) >= 20) || (!c.Terminal && open >= 300) {
					truncated = true
					break
				}
				c.Items = append(c.Items, item)
				if !c.Terminal {
					open++
				}
				break
			}
			if !placed {
				unplaced++
			}
		}
		return boardResult(map[string]any{"workspace": workspace, "collection": col.Slug, "columns": columns, "truncated": truncated, "unplaced": unplaced, "cursor": page.Cursor, "access_epoch": page.AccessEpoch}), nil
	}
}

type boardDoorPage struct {
	Items   []models.Item `json:"items"`
	Changes []struct {
		models.Item
		Deleted  bool `json:"deleted"`
		MovedOut bool `json:"moved_out"`
	} `json:"changes"`
	Cursor      string `json:"cursor"`
	AccessEpoch string `json:"access_epoch"`
}

type boardItem struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	Assignee  string `json:"assignee"`
	ParentRef string `json:"parent_ref"`
	Seq       int64  `json:"seq"`
	UpdatedAt string `json:"updated_at"`
}

type boardColumn struct {
	Status    string      `json:"status"`
	Terminal  bool        `json:"terminal"`
	Abandoned bool        `json:"abandoned"`
	Total     int         `json:"total"`
	Items     []boardItem `json:"items"`
}

func projectBoardItem(it models.Item) boardItem {
	fields := map[string]any{}
	_ = json.Unmarshal([]byte(it.Fields), &fields)
	status, _ := fields["status"].(string)
	priority, _ := fields["priority"].(string)
	return boardItem{ID: it.ID, Ref: it.Ref, Title: it.Title, Status: status, Priority: priority, Assignee: it.AssignedUserName, ParentRef: it.ParentRef, Seq: it.Seq, UpdatedAt: it.UpdatedAt.Format(time.RFC3339Nano)}
}
func decodeBoardText(res *CallToolResult, decode func([]byte) error) error {
	// executeRequest keeps the original REST JSON in the text channel. Its
	// structured channel wraps arrays and parses string-encoded fields/tags;
	// decode the original so the existing models retain their REST shape.
	// Each caller decodes into its own concrete type, so no destination here
	// is an interface (the reserved-field decode guard resolves each one).
	for _, content := range res.Content {
		if text, ok := content.(mcpgo.TextContent); ok {
			return decode([]byte(text.Text))
		}
	}
	return fmt.Errorf("response has no JSON text")
}
func boardResult(value any) *CallToolResult {
	raw, _ := json.Marshal(value)
	return structuredResult(value, string(raw))
}

func boardCollection(res *CallToolResult, slug, key string) (*models.Collection, *CallToolResult) {
	if res.IsError {
		return nil, res
	}
	var cols []models.Collection
	if err := decodeBoardText(res, func(raw []byte) error { return json.Unmarshal(raw, &cols) }); err != nil {
		return nil, dispatcherErrorResult(key, "decode collections", err)
	}
	for _, name := range boardSlugCandidates(slug) {
		for i := range cols {
			if cols[i].Slug == name {
				return &cols[i], nil
			}
		}
	}
	return nil, NewErrorResult(ErrorPayload{Code: ErrNotFound, Message: "Collection not found"})
}

// boardSlugCandidates lists the slugs a show input may name (singular and
// plural forms). Changes takes the slug show returned and matches it exactly.
func boardSlugCandidates(slug string) []string {
	folded := strings.ToLower(strings.TrimSpace(slug))
	return []string{slug, folded, folded + "s", strings.TrimSuffix(folded, "s"), collections.NormalizeSlug(folded)}
}

// boardParentMatches applies the optional parent filter (refs compare
// case-insensitively). An empty filter matches every item.
func boardParentMatches(itemParent, filter string) bool {
	filter = strings.TrimSpace(filter)
	return filter == "" || strings.EqualFold(itemParent, filter)
}
