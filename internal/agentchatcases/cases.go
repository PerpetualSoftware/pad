// Package agentchatcases is the one name table behind the TASK-4186 PR C
// acceptance bar: the client guard (cli.CheckAgentChatTarget) and the server's
// ensure handler must refuse exactly the same rows. The cli and mcp tests and
// the server test all import it, so a row added here is judged by both sides.
// It holds test data only and is imported by tests alone.
package agentchatcases

import (
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Case is one caller (the X-Pad-Agent header) addressing one target.
type Case struct {
	Name   string
	Caller string
	Target string // the chat target as typed: "@x", "C3" or "3"
	// Key is the body agent_key the ensure call would send (the name after
	// "@"). Empty for a C<n> / <n> target, which never reaches ensure.
	Key string
	// Refuse is the shared expectation: the guard AND the server's ensure
	// handler must both refuse this row.
	Refuse bool
}

func dm(name, caller, key string, refuse bool) Case {
	return Case{Name: name, Caller: caller, Target: "@" + key, Key: key, Refuse: refuse}
}

// Cases is the table. The cap rows are built from models.MaxAgentNameRunes.
func Cases() []Case {
	max := models.MaxAgentNameRunes
	a := strings.Repeat("a", max)
	over := a + "bcdef"
	mbFull := strings.Repeat("é", max)
	mbEdge := strings.Repeat("a", max-1) + "é"
	mbOver := mbEdge + "é"
	return []Case{
		dm("exact match", "director", "director", false),
		dm("other agent", "director", "dave", true),
		dm("case difference refuses", "Director", "director", true),
		dm("case difference refuses, other way", "director", "Director", true),
		dm("caller trailing space", "director ", "director", false),
		dm("caller leading space", "  director", "director", false),
		dm("target trailing space is raw", "director", "director ", true),
		dm("target leading space is raw", "director", " director", true),
		dm("caller control char", "dir\tec\x00tor", "director", false),
		dm("target control char is raw", "director", "dir\tector", true),
		dm("caller over cap, truncated target", over, a, false),
		dm("caller over cap, full target", over, over, true),
		dm("multi-byte caller exactly at cap", mbFull, mbFull, false),
		dm("multi-byte caller one rune over cap, truncated target", mbOver, mbEdge, false),
		dm("multi-byte caller one rune over cap, full target", mbOver, mbOver, true),
		dm("multi-byte rune on the boundary", mbEdge, mbEdge, false),
		dm("empty caller is a human", "", "dave", false),
		dm("whitespace-only caller is a human", "  \t ", "dave", false),
		{Name: "C<n> target", Caller: "director", Target: "C3"},
		{Name: "<n> target", Caller: "director", Target: "3"},
	}
}
