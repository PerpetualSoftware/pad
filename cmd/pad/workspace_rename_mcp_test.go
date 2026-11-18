package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestWorkspaceRenameRemoteMCP(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	for _, args := range []string{`{"action":"rename"}`, `{"action":"rename","workspace":"cap-ws"}`, `{"action":"rename","new_slug":"new-cap"}`} {
		if isErr, text := f.mcpToolText(t, f.memberPAT, "pad_workspace", args); !isErr {
			t.Fatalf("accepted %s: %s", args, text)
		}
	}
	tok, err := f.store.CreateAPIToken(f.member.ID, models.APITokenCreate{Name: "pinned", WorkspaceID: f.ws.ID}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other", Slug: "other", OwnerID: f.member.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddWorkspaceMember(other.ID, f.member.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if isErr, text := f.mcpToolText(t, tok.Token, "pad_workspace", `{"action":"rename","workspace":"other","new_slug":"other-new"}`); !isErr {
		t.Fatalf("pinned token renamed other workspace: %s", text)
	}
	if isErr, text := f.mcpToolText(t, f.memberPAT, "pad_workspace", `{"action":"rename","workspace":"cap-ws","new_slug":"new-cap","name":"New Cap"}`); isErr || !strings.Contains(text, "new-cap") {
		t.Fatalf("rename: isError=%v %s", isErr, text)
	}
	for _, slug := range []string{"cap-ws", "new-cap"} {
		if isErr, text := f.mcpToolText(t, tok.Token, "pad_collection", `{"action":"list","workspace":"`+slug+`"}`); isErr {
			t.Fatalf("pin lost access via %s: %s", slug, text)
		}
	}
	if isErr, text := f.mcpToolText(t, tok.Token, "pad_collection", `{"action":"list","workspace":"other"}`); !isErr {
		t.Fatalf("pin gained other workspace: %s", text)
	}
}
