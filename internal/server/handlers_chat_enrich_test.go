package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4039: assertions use wire objects so omitted fields and denial shapes matter.
func newChatEnrichFixture(t *testing.T) *chatFixture {
	t.Helper()
	f := newChatFixture(t)
	f.owner = f.member("chat-reader@example.com", "editor")
	f.ownerTok = f.token(f.owner)
	return f
}

func chatRefs(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	values, ok := chatObject(t, rr)["refs"].([]any)
	if !ok {
		t.Fatalf("refs must be an array: %s", rr.Body.String())
	}
	out := make([]map[string]any, 0, len(values))
	for _, v := range values {
		out = append(out, v.(map[string]any))
	}
	return out
}

func TestChat_Refs_TruncatedAt200(t *testing.T) {
	for _, count := range []int{0, 10, 200, 6000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newChatEnrichFixture(t)
			p := f.ensure("rook")
			check := func(rr *httptest.ResponseRecorder, want int, truncated bool) {
				t.Helper()
				refs := chatRefs(t, rr)
				if len(refs) != want {
					t.Errorf("len(refs) = %d, want %d", len(refs), want)
				}
				if got := chatObject(t, rr)["refs_truncated"]; got != truncated {
					t.Errorf("refs_truncated = %v, want %v (always present)", got, truncated)
				}
				resolved := 0
				for i, ref := range refs {
					if ref["workspace"] != "ws" || ref["ref"] != fmt.Sprintf("TASK-%d", i+1) {
						t.Fatalf("ref %d = %v; want distinct refs in source order", i, ref)
					}
					if ref["state"] != "unresolved" {
						resolved++
					}
				}
				if resolved > 50 {
					t.Errorf("%d non-unresolved refs, want at most 50", resolved)
				}
			}
			// TASK-4039 r2: split 6,000 refs into bodies below the 64 KiB cap.
			for start := 1; start <= count; start += 2000 {
				var body strings.Builder
				end := min(start+1999, count)
				for i := start; i <= end; i++ {
					fmt.Fprintf(&body, "[[ws::TASK-%d]] ", i)
				}
				// TASK-4039 r2: duplicates at the cap do not imply truncation.
				fmt.Fprintf(&body, "[[ws::task-%d]]", start)
				rr := f.request("POST", p+"/messages", "", map[string]any{"body": body.String(), "op_key": fmt.Sprint(start)})
				f.must(rr, 201, "send refs")
				if start == 1 {
					check(rr, min(end, 200), end > 200)
					replay := f.request("POST", p+"/messages", "", map[string]any{"body": "replay", "op_key": fmt.Sprint(start), "context_workspace": "missing"})
					f.must(replay, 200, "replay with denied context")
					check(replay, min(end, 200), end > 200)
				}
			}
			if count == 200 {
				f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "[[ws::task-1]] [[ws::TASK-200]]"}), 201, "duplicate-only message")
			}
			page := f.request("GET", p+"/messages?limit=200", "", nil)
			f.must(page, 200, "list refs")
			check(page, min(count, 200), count > 200)
		})
	}
}

func TestChat_ContextWorkspace_ShownThenHiddenAfterMemberRemoved(t *testing.T) {
	f := newChatEnrichFixture(t)
	p := f.ensure("rook")
	sent := f.request("POST", p+"/messages", "", map[string]any{"body": "hello", "context_workspace": f.wsSlug})
	f.must(sent, 201, "send")
	stored, err := f.srv.store.ListConversationMessages(models.ConversationActor{UserID: f.owner.ID}, chatMessage(t, sent)["conversation_id"].(string), nil, nil, 50)
	if err != nil || len(stored.Messages) != 1 || stored.Messages[0].ContextWorkspaceID == nil || *stored.Messages[0].ContextWorkspaceID != f.wsID {
		t.Fatalf("context not persisted: %+v, %v", stored, err)
	}
	before := f.request("GET", p+"/messages", "", nil)
	f.must(before, 200, "before removal")
	message := chatObject(t, before)["messages"].([]any)[0].(map[string]any)
	wantContext := map[string]any{"slug": f.wsSlug, "name": "Access WS"}
	if !reflect.DeepEqual(message["context_workspace"], wantContext) || !reflect.DeepEqual(chatMessage(t, sent)["context_workspace"], wantContext) {
		t.Fatalf("visible context: %v", message)
	}
	delete(message, "context_workspace")
	if err := f.srv.store.RemoveWorkspaceMember(f.wsID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	after := f.request("GET", p+"/messages", "", nil)
	f.must(after, 200, "after removal")
	got := chatObject(t, after)["messages"].([]any)[0].(map[string]any)
	if _, ok := got["context_workspace"]; ok {
		t.Errorf("context_workspace present after member removal: %v", got)
	}
	if strings.Contains(after.Body.String(), f.wsSlug) || strings.Contains(after.Body.String(), f.wsID) {
		t.Errorf("hidden workspace leaked: %s", after.Body.String())
	}
	if !reflect.DeepEqual(message, got) {
		t.Errorf("message otherwise changed: before %v after %v", message, got)
	}
	plain := f.request("POST", p+"/messages", "", map[string]any{"body": "hello"})
	f.must(plain, 201, "send without context")
	// TASK-4039: normalize message identity to pin byte-identical omission.
	a, b := got, chatMessage(t, plain)
	for _, m := range []map[string]any{a, b} {
		delete(m, "id")
		delete(m, "seq")
		delete(m, "created_at")
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("hidden context differs from no context: %s / %s", aj, bj)
	}
}

func TestChat_ContextWorkspace_SoftDeletedWorkspaceHidden(t *testing.T) {
	f := newChatEnrichFixture(t)
	p := f.ensure("rook")
	sent := f.request("POST", p+"/messages", "", map[string]any{"body": "hello", "context_workspace": f.wsID})
	f.must(sent, 201, "send by id")
	if chatMessage(t, sent)["context_workspace"] == nil {
		t.Fatal("visible context missing before workspace deletion")
	}
	if err := f.srv.store.DeleteWorkspace(f.wsSlug); err != nil {
		t.Fatal(err)
	}
	rr := f.request("GET", p+"/messages", "", nil)
	f.must(rr, 200, "get")
	if strings.Contains(rr.Body.String(), "context_workspace") || strings.Contains(rr.Body.String(), f.wsID) || strings.Contains(rr.Body.String(), f.wsSlug) {
		t.Fatalf("soft-deleted context leaked: %s", rr.Body.String())
	}
	rr = f.request("DELETE", p+"/messages/1", "", nil)
	f.must(rr, 200, "delete")
	if strings.Contains(rr.Body.String(), "context_workspace") {
		t.Fatalf("tombstone leaks context: %s", rr.Body.String())
	}
}

func TestChat_ContextWorkspace_BadValueIs400(t *testing.T) {
	f := newChatEnrichFixture(t)
	p := f.ensure("rook")
	other := mkUser(t, f.srv, "other-context@example.com")
	private := mustWorkspace(t, f.srv, "Private context", other.ID)
	gone := mustWorkspace(t, f.srv, "Deleted context", f.owner.ID)
	if err := f.srv.store.DeleteWorkspace(gone.Slug); err != nil {
		t.Fatal(err)
	}
	var want string
	for _, value := range []string{"no-such-context", private.Slug, private.ID, gone.Slug, gone.ID} {
		rr := f.request("POST", p+"/messages", "", map[string]any{"body": "hello", "context_workspace": value})
		f.must(rr, 400, value)
		if want == "" {
			want = rr.Body.String()
		}
		if rr.Body.String() != want {
			t.Errorf("context denial differs: %s / %s", want, rr.Body.String())
		}
		err := chatObject(t, rr)["error"].(map[string]any)
		if err["code"] != "validation_error" || err["message"] != "context workspace not found" {
			t.Fatal(err)
		}
	}
	// TASK-4039: workspace visibility alone does not satisfy the store's membership gate.
	coll := mustCollection(t, f.srv, private.ID, "Guest access")
	item := mustItem(t, f.srv, private.ID, coll.ID, "Guest item")
	if _, err := f.srv.store.CreateItemGrant(private.ID, item.ID, f.owner.ID, "view", other.ID); err != nil {
		t.Fatal(err)
	}
	rr := f.request("POST", p+"/messages", "", map[string]any{"body": "hello", "context_workspace": private.ID})
	f.must(rr, 400, "guest context")
	if rr.Body.String() != want {
		t.Fatalf("store context denial differs: %s / %s", want, rr.Body.String())
	}
}

func TestChat_Refs_VisibleRestrictedUnresolved(t *testing.T) {
	t.Run("visibility and extraction", func(t *testing.T) {
		f := newChatEnrichFixture(t)
		p := f.ensure("rook")
		visible := f.seedItem()
		coll, err := f.srv.store.GetCollection(visible.CollectionID)
		if err != nil {
			t.Fatal(err)
		}
		secret := mustCollection(t, f.srv, f.wsID, "Secret")
		hidden := mustItem(t, f.srv, f.wsID, secret.ID, "Hidden title")
		if err := f.srv.store.SetMemberCollectionAccess(f.wsID, f.owner.ID, "specific", []string{coll.ID}); err != nil {
			t.Fatal(err)
		}
		leftOwner := mkUser(t, f.srv, "left-owner@example.com")
		left := mustWorkspace(t, f.srv, "Left workspace", leftOwner.ID)
		if err := f.srv.store.AddWorkspaceMember(left.ID, f.owner.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		leftColl := mustCollection(t, f.srv, left.ID, "Tasks")
		leftItem := mustItem(t, f.srv, left.ID, leftColl.ID, "Left title")
		if err := f.srv.store.RemoveWorkspaceMember(left.ID, f.owner.ID); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("[[%s::%s]] [[%s::%s]] [[%s::%s]] [[%s::TASK-999999]] [[no-such-workspace::TASK-1]] [[%s]] [[Title]] [[tasks/Title]]\n\x60\x60\x60\n[[%s::TASK-888888]]\n\x60\x60\x60\n\x60[[%s::TASK-777777]]\x60 [[%s::%s]]",
			f.wsSlug, visible.Ref, f.wsSlug, hidden.Ref, left.Slug, leftItem.Ref, f.wsSlug, visible.Ref, f.wsSlug, f.wsSlug, f.wsSlug, strings.ToLower(visible.Ref))
		rr := f.request("POST", p+"/messages", "", map[string]any{"body": body, "context_workspace": f.wsSlug})
		f.must(rr, 201, "send refs")
		refs := chatRefs(t, rr)
		if len(refs) != 5 {
			t.Fatalf("dedupe/title/code extraction: %v", refs)
		}
		if refs[0]["state"] != "visible" || refs[0]["title"] != visible.Title || refs[0]["collection_slug"] != coll.Slug {
			t.Fatalf("visible ref: %v", refs[0])
		}
		var shape string
		for _, ref := range refs[1:] {
			entry := make(map[string]any)
			for k, v := range ref {
				if k != "ref" && k != "workspace" {
					entry[k] = v
				}
			}
			raw, _ := json.Marshal(entry)
			if ref["state"] != "restricted" || (shape != "" && shape != string(raw)) {
				t.Errorf("restricted entries differ: want %s got %s (%v)", shape, raw, ref)
			}
			shape = string(raw)
		}
		page := f.request("GET", p+"/messages", "", nil)
		f.must(page, 200, "list refs")
		if !reflect.DeepEqual(chatRefs(t, page), refs) {
			t.Fatal("send and list refs differ")
		}
		rr = f.request("POST", p+"/messages", "", map[string]any{"body": "[[" + visible.Ref + "]]"})
		f.must(rr, 201, "no context")
		bare := chatRefs(t, rr)
		if len(bare) != 1 || bare[0]["state"] != "unresolved" || bare[0]["workspace"] != "" {
			t.Fatalf("bare without context: %v", bare)
		}
		inbox := f.request("GET", chatBase, "", nil)
		f.must(inbox, 200, "inbox")
		last := chatObject(t, inbox)["conversations"].([]any)[0].(map[string]any)["last_message"].(map[string]any)
		for _, key := range []string{"body", "context_workspace", "refs"} {
			if _, ok := last[key]; ok {
				t.Fatalf("inbox enrichment: %v", last)
			}
		}
		for _, seq := range []string{"1", "2"} {
			deleted := f.request("DELETE", p+"/messages/"+seq, "", nil)
			f.must(deleted, 200, "delete")
			if _, ok := chatMessage(t, deleted)["context_workspace"]; ok {
				t.Fatal("deleted message has context")
			}
		}
		rr = f.request("GET", p+"/messages", "", nil)
		f.must(rr, 200, "tombstones")
		if refs := chatRefs(t, rr); len(refs) != 0 {
			t.Fatalf("tombstones yield refs: %v", refs)
		}
	})
	t.Run("cap", func(t *testing.T) {
		f := newChatEnrichFixture(t)
		p := f.ensure("rook")
		var body strings.Builder
		for i := 1; i <= 51; i++ {
			fmt.Fprintf(&body, "[[missing::TASK-%d]] ", i)
		}
		rr := f.request("POST", p+"/messages", "", map[string]any{"body": body.String()})
		f.must(rr, 201, "send cap")
		refs := chatRefs(t, rr)
		if len(refs) != 51 || refs[49]["state"] != "restricted" || refs[50]["state"] != "unresolved" {
			t.Fatalf("50-ref cap: %v", refs)
		}
		f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "[[missing::TASK-1]] [[missing::TASK-52]]"}), 201, "second message")
		rr = f.request("GET", p+"/messages", "", nil)
		f.must(rr, 200, "list cap")
		if refs := chatRefs(t, rr); len(refs) != 52 || refs[50]["state"] != "unresolved" || refs[51]["state"] != "unresolved" {
			t.Fatalf("list overflow must be unresolved across messages: %v", refs)
		}
	})
}

func TestChat_Refs_TokenAllowListHonoured(t *testing.T) {
	f := newChatEnrichFixture(t)
	p := f.ensure("rook")
	item := f.seedItem()
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": fmt.Sprintf("[[%s::%s]]", f.wsSlug, item.Ref), "context_workspace": f.wsSlug}), 201, "send")
	r := httptest.NewRequest("GET", p+"/messages", nil)
	r.Header.Set("Authorization", "Bearer "+f.ownerTok)
	r = r.WithContext(WithTokenAllowedWorkspaces(r.Context(), []string{"another-workspace"}))
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, r)
	f.must(rr, 200, "allow-list get")
	refs := chatRefs(t, rr)
	if len(refs) != 1 || refs[0]["state"] != "restricted" || refs[0]["title"] != nil {
		t.Fatalf("allow-list ref leak: %v", refs)
	}
	m := chatObject(t, rr)["messages"].([]any)[0].(map[string]any)
	if _, ok := m["context_workspace"]; ok {
		t.Fatalf("allow-list context leak: %v", m)
	}
}

// TASK-4039 r1: bare refs without context do not consume lookup slots.
func TestChat_Refs_ContextlessRefsDoNotConsumeCap(t *testing.T) {
	f := newChatEnrichFixture(t)
	p := f.ensure("rook")
	item := f.seedItem()
	var body strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&body, "[[TASK-%d]] ", i)
	}
	fmt.Fprintf(&body, "[[%s::%s]]", f.wsSlug, item.Ref)
	rr := f.request("POST", p+"/messages", "", map[string]any{"body": body.String()})
	f.must(rr, 201, "send mixed refs")
	refs := chatRefs(t, rr)
	if len(refs) != 51 || refs[50]["state"] != "visible" || refs[50]["title"] != item.Title {
		t.Fatalf("real ref pushed past cap: %v", refs)
	}
	for _, ref := range refs[:50] {
		if ref["state"] != "unresolved" || ref["workspace"] != "" {
			t.Fatalf("contextless ref: %v", ref)
		}
	}
}
