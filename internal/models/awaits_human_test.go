package models

import "testing"

// TASK-3919 (IDEA-3906): awaits_human reads false when absent, is carried
// across a settings write that does not mention it, and refuses a non-bool.
func TestAwaitsHuman_ReadCarrySetValidate(t *testing.T) {
	for in, want := range map[string]bool{
		"": false, "{}": false, "not json": false,
		`{"awaits_human":true}`: true, `{"awaits_human":false}`: false,
	} {
		if got := CollectionAwaitsHumanJSON(in); got != want {
			t.Errorf("CollectionAwaitsHumanJSON(%q) = %v, want %v", in, got, want)
		}
	}

	// A stray non-boolean (an import is not validated) reads as false and
	// leaves every other setting readable: tracks_work keeps its stored false.
	stray := `{"tracks_work":false,"awaits_human":"yes"}`
	if CollectionAwaitsHumanJSON(stray) || CollectionTracksWorkJSON(stray) {
		t.Errorf("a stray awaits_human broke the settings read: awaits %v, work %v",
			CollectionAwaitsHumanJSON(stray), CollectionTracksWorkJSON(stray))
	}

	stored := `{"awaits_human":true,"layout":"list"}`
	if got := CarryAwaitsHuman(`{"layout":"board"}`, stored); !CollectionAwaitsHumanJSON(got) {
		t.Errorf("a write without the key dropped the stored true: %s", got)
	}
	if got := CarryAwaitsHuman(`{"awaits_human":false}`, stored); CollectionAwaitsHumanJSON(got) {
		t.Errorf("a write that says false was overridden by the stored true: %s", got)
	}
	if got := CarryAwaitsHuman(`{"layout":"board"}`, `{}`); got != `{"layout":"board"}` {
		t.Errorf("nothing stored, but the write changed: %s", got)
	}

	set, err := SetAwaitsHuman(`{"layout":"list"}`, true)
	if err != nil || !CollectionAwaitsHumanJSON(set) || CollectionTracksWorkJSON(set) != true {
		t.Fatalf("SetAwaitsHuman: %s, %v", set, err)
	}
	if _, err := SetAwaitsHuman(`[1]`, true); err == nil {
		t.Error("SetAwaitsHuman accepted settings that are not an object")
	}

	for in, ok := range map[string]bool{
		"": true, "{}": true, `{"awaits_human":true}`: true, `{"awaits_human":false}`: true,
		`{"awaits_human":"yes"}`: false, `{"awaits_human":1}`: false, `{"awaits_human":null}`: false,
	} {
		if err := ValidateAwaitsHumanSetting(in); (err == nil) != ok {
			t.Errorf("ValidateAwaitsHumanSetting(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}
