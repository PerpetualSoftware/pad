package cli

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func directionFileFields(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func writeDirectionFileFields(t *testing.T, path string, fields map[string]any) {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestChatDirectionArmGeneration(t *testing.T) {
	directionEnv(t)
	armPath, err := WriteArmState()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := directionFileFields(t, armPath)["generation"].(string)
	nonce, err := hex.DecodeString(first)
	if err != nil || len(nonce) != 32 {
		t.Errorf("arm must write a 32-byte random generation: %q", first)
	}
	path, err := WriteChatDirection()
	if err != nil {
		t.Fatal(err)
	}
	if directionFileFields(t, path)["generation"] != first || !ChatDirectionOn() {
		t.Error("normal chat arm must copy this arm's generation and enable direction")
	}
	if _, err := MarkFirstConnect(); err != nil || !ChatDirectionOn() {
		t.Fatal("boot flag rewrite must preserve generation", err)
	}
	if _, err := WriteDisarmState(); err != nil {
		t.Fatal(err)
	}
	if ChatDirectionOn() {
		t.Fatal("disarmed direction on")
	}
	// Deliberately retain the direction file, as with a failed remover.
	if _, err := WriteArmState(); err != nil {
		t.Fatal(err)
	}
	second, _ := directionFileFields(t, armPath)["generation"].(string)
	if second == "" || second == first {
		t.Error("re-arm must issue a fresh generation")
	}
	if ChatDirectionOn() {
		t.Error("surviving direction file revived after re-arm")
	}
	if _, err := WriteChatDirection(); err != nil || !ChatDirectionOn() {
		t.Fatal("explicit chat arm did not enable new generation", err)
	}
}

func TestChatDirectionMissingGenerationFailsClosed(t *testing.T) {
	for _, missing := range []string{"direction", "arm", "both"} {
		t.Run(missing, func(t *testing.T) {
			directionEnv(t)
			armPath, err := WriteArmState()
			if err != nil {
				t.Fatal(err)
			}
			path, err := WriteChatDirection()
			if err != nil {
				t.Fatal(err)
			}
			for name, file := range map[string]string{"arm": armPath, "direction": path} {
				if missing == "both" || missing == name {
					fields := directionFileFields(t, file)
					delete(fields, "generation")
					writeDirectionFileFields(t, file, fields)
				}
			}
			if SessionArmState() != LocalArmOn || !ResolveAnnouncedArmed() {
				t.Fatal("old-format arm state must still authorize session delivery")
			}
			if ChatDirectionOn() {
				t.Fatal("missing generation enabled direction")
			}
		})
	}
}
