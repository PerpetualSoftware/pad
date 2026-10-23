package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
)

type chatContextWorkspace struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type chatRef struct {
	Workspace      string `json:"workspace"`
	Ref            string `json:"ref"`
	State          string `json:"state"`
	Title          string `json:"title,omitempty"`
	CollectionSlug string `json:"collection_slug,omitempty"`
}

// TASK-4039: visibility belongs to this reader and request, never the stored message.
func (s *Server) enrichChatMessages(r *http.Request, actor models.ConversationActor, messages []models.ConversationMessage) ([]chatMsg, []chatRef, bool) {
	out := make([]chatMsg, 0, len(messages))
	refs := make([]chatRef, 0)
	contexts := make(map[string]*chatContextWorkspace)
	type refKey struct{ workspace, ref string }
	seen := make(map[refKey]bool)
	lookups := 0
	truncated := false
	for _, m := range messages {
		msg := newChatMsg(actor, m)
		if m.Deleted {
			out = append(out, msg)
			continue
		}
		if m.ContextWorkspaceID != nil {
			id := *m.ContextWorkspaceID
			context, checked := contexts[id]
			if !checked {
				access := s.AuthorizeCrossWorkspaceRead(r, id, CrossWorkspaceWorkspaceOnlyScope())
				if access.Allowed {
					context = &chatContextWorkspace{Slug: access.Workspace.Slug, Name: access.Workspace.Name}
				}
				contexts[id] = context
			}
			msg.ContextWorkspace = context
		}
		out = append(out, msg)
		// TASK-4039 r2: after the first drop, later bodies cannot add any refs.
		if truncated {
			continue
		}
		for _, link := range links.ExtractWikiLinks(m.Body) {
			if link.Kind != links.WikiLinkKindRef && link.Kind != links.WikiLinkKindWorkspaceRef {
				continue
			}
			workspace := link.WorkspaceSlug
			if link.Kind == links.WikiLinkKindRef && msg.ContextWorkspace != nil {
				workspace = msg.ContextWorkspace.Slug
			}
			ref := chatRef{Workspace: workspace, Ref: links.CanonicalizeRef(link.Ref), State: "unresolved"}
			key := refKey{workspace, ref.Ref}
			if seen[key] {
				continue
			}
			// TASK-4039 r2: duplicates at the cap do not imply a dropped ref.
			if len(refs) == 200 {
				truncated = true
				break
			}
			seen[key] = true
			// TASK-4039 r1: context-less refs spend no lookup slot; overflow stays unresolved.
			if workspace != "" && lookups < 50 {
				lookups++
				ref = s.resolveChatRef(r, ref)
			}
			refs = append(refs, ref)
		}
	}
	return out, refs, truncated
}

// TASK-4039: absence and every denial share the exact same restricted projection.
func (s *Server) resolveChatRef(r *http.Request, ref chatRef) chatRef {
	ref.State = "restricted"
	prefix, number, ok := parseRefForRedirect(ref.Ref)
	if !ok {
		return ref
	}
	ws, err := s.resolveWorkspace(ref.Workspace, currentUser(r))
	if err != nil || ws == nil {
		return ref
	}
	item, err := s.store.GetItemByRef(ws.ID, prefix, number)
	if err != nil || item == nil {
		return ref
	}
	access := s.AuthorizeCrossWorkspaceRead(r, ref.Workspace, CrossWorkspaceItemScope(item))
	if !access.Allowed {
		return ref
	}
	ref.State = "visible"
	ref.Title = item.Title
	ref.CollectionSlug = item.CollectionSlug
	return ref
}
