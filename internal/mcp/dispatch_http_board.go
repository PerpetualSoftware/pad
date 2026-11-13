package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/url"
	"slices"
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
			if err := decodeBoardResult(res, &page); err != nil {
				return dispatcherErrorResult(key, "decode changes", err), nil
			}
			upserts := []boardItem{}
			removed := []string{}
			parent, _ := input["parent"].(string)
			for _, row := range page.Changes {
				if !row.Deleted && !row.MovedOut && boardSlugMatches(row.CollectionSlug, collection) && boardParentMatches(row.ParentRef, parent) {
					upserts = append(upserts, projectBoardItem(row.Item))
				} else {
					removed = append(removed, row.ID)
				}
			}
			return boardResult(map[string]any{"upserts": upserts, "removed_ids": removed, "cursor": page.Cursor, "access_epoch": page.AccessEpoch, "resync": page.AccessEpoch != input["access_epoch"] || len(page.Changes) == 500}), nil
		}
		col, failure := boardCollection(get(base+"/collections"), collection, key)
		if failure != nil {
			return failure, nil
		}
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(col.Schema), &schema); err != nil {
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
		if err := decodeBoardResult(res, &page); err != nil {
			return dispatcherErrorResult(key, "decode index", err), nil
		}
		columns := make([]boardColumn, 0, len(status.Options))
		for _, option := range status.Options {
			terminal := slices.Contains(status.TerminalOptions, option)
			abandoned := false
			if terminal {
				if len(status.AbandonedOptions) > 0 {
					for _, v := range status.AbandonedOptions {
						if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(option)) {
							abandoned = true
						}
					}
				} else {
					abandoned = models.IsNegativeTerminal(option)
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
func decodeBoardResult(res *CallToolResult, out any) error {
	// executeRequest keeps the original REST JSON in the text channel. Its
	// structured channel wraps arrays and parses string-encoded fields/tags;
	// decode the original so the existing models retain their REST shape.
	for _, content := range res.Content {
		if text, ok := content.(mcpgo.TextContent); ok {
			return json.Unmarshal([]byte(text.Text), out)
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
	if err := decodeBoardResult(res, &cols); err != nil {
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

// boardSlugCandidates lists the slugs a collection input may name, so show
// and changes accept the same singular and plural forms.
func boardSlugCandidates(slug string) []string {
	folded := strings.ToLower(strings.TrimSpace(slug))
	return []string{slug, folded, folded + "s", strings.TrimSuffix(folded, "s"), collections.NormalizeSlug(folded)}
}

// boardSlugMatches reports whether a change row's collection is the board's.
// It does not tell apart two collections whose slugs differ only by a
// trailing s; the view passes back the slug show returned.
func boardSlugMatches(rowSlug, input string) bool {
	return rowSlug != "" && slices.Contains(boardSlugCandidates(input), rowSlug)
}

// boardParentMatches applies the optional parent filter (refs compare
// case-insensitively). An empty filter matches every item.
func boardParentMatches(itemParent, filter string) bool {
	filter = strings.TrimSpace(filter)
	return filter == "" || strings.EqualFold(itemParent, filter)
}
