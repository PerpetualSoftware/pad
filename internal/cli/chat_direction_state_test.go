package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func directionEnv(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	armStateTestEnv(t, socket)
	return socket
}
func TestStreamIdentityChatParam(t *testing.T) {
	c := NewClientFromURL("http://localhost:7777")
	for _, chat := range []bool{false, true} {
		r, err := c.NewWatchEventsStreamRequest(context.Background(), "", StreamSessionIdentity{Chat: chat})
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.Query().Has("chat") != chat || chat && r.URL.Query().Get("chat") != "true" {
			t.Fatal(r.URL)
		}
	}
}
func TestChatDirectionFileKeyedBySocketOnly(t *testing.T) {
	socket := directionEnv(t)
	if _, err := WriteArmState(); err != nil {
		t.Fatal(err)
	}
	path, err := WriteChatDirection()
	if err != nil {
		t.Fatal(err)
	}
	if !ChatDirectionOn() {
		t.Fatal("not on")
	}
	cwd, _ := os.Getwd()
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	if _, err := WriteArmState(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	dir := filepath.Dir(path)
	for _, name := range []string{"chat-direction-dir-" + shortHash(cwd) + ".json", "chat-direction-repo-" + shortHash(cwd) + ".json"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteChatDirection(); err == nil {
		t.Fatal("headless write")
	}
	if ChatDirectionOn() {
		t.Fatal("cwd fallback")
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket+"other")
	if ChatDirectionOn() {
		t.Fatal("shared key")
	}
}
func TestChatDirectionLivenessAndReaping(t *testing.T) {
	socket := directionEnv(t)
	WriteArmState()
	path, err := WriteChatDirection()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := ownerLivenessFn
	t.Cleanup(func() { ownerLivenessFn = orig })
	checks := 0
	ownerLivenessFn = func(*SessionOwner) Liveness {
		checks++
		if checks == 1 {
			return LivenessUnknown
		}
		return LivenessAlive
	}
	if ChatDirectionOn() {
		t.Fatal("unknown on")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown reaped", err)
	}
	ownerLivenessFn = orig
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if ChatDirectionOn() {
		t.Fatal("dead on")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dead not reaped", err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if ChatDirectionOn() {
		t.Fatal("malformed on")
	}
	var st ArmState
	json.Unmarshal(data, &st)
	st.Socket = "another"
	b, _ := json.Marshal(st)
	os.WriteFile(path, b, 0600)
	if ChatDirectionOn() {
		t.Fatal("another session on")
	}
}
func TestChatDirectionNeedsArmedSession(t *testing.T) {
	directionEnv(t)
	WriteArmState()
	WriteChatDirection()
	if !ChatDirectionOn() {
		t.Fatal("off")
	}
	WriteDisarmState()
	if ChatDirectionOn() {
		t.Fatal("disarmed on")
	}
}
func TestPadTomlNeverImpliesDirection(t *testing.T) {
	// Read before directionEnv changes cwd.
	src, err := os.ReadFile("chat_direction_state.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"LoadPadToml", "ResolveAutoArm", "auto_arm"} {
		if strings.Contains(string(src), s) {
			t.Fatalf("forbidden dependency %s", s)
		}
	}
	directionEnv(t)
	os.WriteFile(".pad.toml", []byte("workspace = 'test'\n[push]\nauto_arm = true\n"), 0600)
	if ChatDirectionOn() {
		t.Fatal("repo implied direction")
	}
	WriteChatDirection()
	if ChatDirectionOn() {
		t.Fatal("repo arm substituted for explicit arm")
	}
}
