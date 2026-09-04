package cli

import "os"

// ResolveAgentName decides what goes in the X-Pad-Agent header, which is the
// ONLY signal the server has for attributing a write to an agent rather than a
// human (server.actorFromRequest). Before BUG-2542 the header was populated
// from exactly one place — `agent_name` in .pad.toml — and nothing else, so any
// workspace that had not opted in recorded every agent write as a human one.
// The embedded skill meanwhile promised that `created_by: agent` was automatic.
// The contract was the thing that was wrong; this makes it true instead.
//
// Precedence, most explicit first:
//
//  1. The agent this SESSION registered as (`pad session register --agent`),
//     when a live registry record for the owning session exists and names
//     one — the most recent and most deliberate declaration, and the one
//     `pad session list` shows. BUG-2882: before this step the registry and
//     the write stamp were two self-declarations that could disagree, and
//     did — two seats launched under one name, one re-registered under the
//     right one, and every write it made afterwards carried the wrong one,
//     because `--agent` could rewrite the registry row but not $PAD_AGENT,
//     and nothing reconciled the two. Now the row IS the stamp. An anonymous
//     registration (`--agent ""`) does not blank a name the environment
//     declares; it only says the registry row is anonymous.
//  2. `agent_name` in .pad.toml — a deliberate per-workspace choice.
//  3. $PAD_AGENT — the runtime-agnostic override. Any harness we have not
//     taught this function about can set it and be attributed correctly.
//  4. A detected agent runtime, below.
//
// WHAT THIS IS NOT. The header is client-supplied and self-declared, so it
// records honesty, not identity:
//
//   - an agent that omits it is indistinguishable from the human whose
//     credentials it is using;
//   - a human running `! pad ...` inside an agent's terminal inherits that
//     terminal's environment and will be attributed to the agent.
//
// So this is not a basis for machine-verifiable human-approval provenance. A
// grant that has to be provable needs a channel the agent cannot author at all.
// BUG-2542's trail has the incident that made the distinction concrete: an
// agent's relay of a human's words was recorded indistinguishably from the
// human having typed them.
func ResolveAgentName() string {
	if name, ok := registeredAgentForThisSession(); ok && name != "" {
		return name
	}
	if pt, _ := LoadPadToml(); pt != nil && pt.AgentName != "" {
		return pt.AgentName
	}
	if name := os.Getenv("PAD_AGENT"); name != "" {
		return name
	}
	return detectAgentRuntime()
}

// agentRuntimeEnv maps an environment variable to the agent name recorded when
// it is set to a non-empty value.
//
// Only entries VERIFIED against a live session of that runtime belong here.
// Claude Code exports CLAUDECODE=1 to child processes (confirmed by reading the
// environment of a `pad` subprocess inside one). Other harnesses — Cursor,
// Windsurf, Aider, Codex — very likely have their own markers, but guessing at
// variable names would put unverified claims in a shipped binary and produce
// silent misattribution when wrong in either direction. They should set
// $PAD_AGENT until someone confirms a signature and adds it here with the same
// standard of evidence.
var agentRuntimeEnv = []struct {
	env  string
	name string
}{
	{env: "CLAUDECODE", name: "claude-code"},
}

func detectAgentRuntime() string {
	for _, r := range agentRuntimeEnv {
		if os.Getenv(r.env) != "" {
			return r.name
		}
	}
	return ""
}

// ActorKind is the writer role — "agent" or "user" — as this process can best
// describe itself. Same self-declared signal as ResolveAgentName, just reduced
// to the enum the created_by / last_modified_by fields hold.
//
// Use it ONLY where the client is the last party that could know: structured
// entries embedded in an item's fields JSON (implementation notes, decision
// log) are written whole by the CLI and the server never parses them for
// attribution, so nobody downstream can fill this in. Everywhere else, leave
// attribution empty and let the server stamp it from the request — an explicit
// value suppresses that stamp, which is how the CLI used to record every agent
// note as a human one (BUG-2542).
func ActorKind() string {
	if ResolveAgentName() != "" {
		return "agent"
	}
	return "user"
}
