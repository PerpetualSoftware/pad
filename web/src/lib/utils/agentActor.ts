/**
 * The agent display name carried on a workspace activity row.
 *
 * WHERE THE NAME COMES FROM. The CLI resolves it once per process
 * (`internal/cli/agent_identity.go::ResolveAgentName`: `agent_name` in
 * .pad.toml, then $PAD_AGENT, then a detected runtime) and sends it as
 * `X-Pad-Agent`. The server stamps it into the activity's metadata as
 * `agent` (`handlers_documents.go::agentMeta`), reached from
 * `logActivityWithMetaReturningID` — so **workspace activity rows are the
 * only thing in the data model that carries it**. Comments, versions,
 * items, structured note/decision entries and SSE events all record the
 * actor KIND ("agent" / "user") and no name; a surface holding one of
 * those has nothing to render here and says "agent" as it always did.
 *
 * VERBATIM IS THE WHOLE CONTRACT. Whatever an agent sent is what a reader
 * sees — no allow-list, no normalization, no title-casing. Pad does not
 * know which client ids are "real names": a run of seats calling
 * themselves `claude-code` and a run calling themselves `wren` are the
 * same fact reported at different resolutions, and the display's job is to
 * report it, not to grade it. This replaces a shim that filtered a
 * hardcoded set of one team's client ids out of the Live view, which made
 * the feature's display quality depend on that team's naming habits
 * (CONVE-2757: no workspace's tool names in product logic, display filters
 * included). Historical rows stamped `claude-code` render `claude-code`.
 *
 * WHAT IT DOES NOT CLAIM. The header is client-supplied and self-declared,
 * so a name here records honesty, not identity — see the "WHAT THIS IS
 * NOT" section of ResolveAgentName's doc comment for the two ways it can
 * be wrong. Nothing rendered from this function is evidence about who
 * acted; it is a label the actor chose.
 */

/**
 * The agent name stamped on an activity's metadata, or undefined when the
 * row carries none (a human's write, a pre-BUG-2542 row, an agent that
 * never sent the header) or the metadata is unparseable.
 *
 * `metadata` is the raw JSON string as the API returns it — callers that
 * have already parsed it for other fields can use {@link agentNameOf}
 * instead of parsing twice.
 */
export function agentNameFromMetadata(metadata: string | undefined | null): string | undefined {
	if (!metadata) return undefined;
	try {
		return agentNameOf(JSON.parse(metadata) as Record<string, unknown>);
	} catch {
		return undefined;
	}
}

/** {@link agentNameFromMetadata} for callers holding the parsed object. */
export function agentNameOf(metadata: Record<string, unknown> | undefined | null): string | undefined {
	const name = metadata?.agent;
	// A non-string cannot occur through agentMeta (it marshals map[string]string),
	// but this parses untrusted JSON off the wire, so it is checked rather than
	// assumed — an object here would otherwise render as "[object Object]".
	if (typeof name !== 'string' || name.length === 0) return undefined;
	return name;
}

/**
 * The label for an agent actor: its stamped name, or `fallback` when the
 * row carries none.
 *
 * The fallback stays the caller's, because each surface already has its
 * own vocabulary for the nameless case ("agent" in the feed's lowercase
 * badges, "Agent" in the timeline's chips) and this change is not the
 * place to unify them.
 */
export function agentActorLabel(
	metadata: string | undefined | null,
	fallback: string
): string {
	return agentNameFromMetadata(metadata) ?? fallback;
}
