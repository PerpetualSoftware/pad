/**
 * The agent display name carried on a workspace activity row.
 *
 * WHERE THE NAME COMES FROM. The CLI resolves it once per process
 * (`internal/cli/agent_identity.go::ResolveAgentName`: `agent_name` in
 * .pad.toml, then $PAD_AGENT, then a detected runtime) and sends it as
 * `X-Pad-Agent`. The server stamps it into the activity's metadata as
 * `agent` (`handlers_documents.go::agentMeta`), reached from
 * `logActivityWithMetaReturningID` — so **workspace activity rows are the
 * only thing in the data model that STORES it**. A comment reaches it
 * indirectly: the store's comment list queries LEFT JOIN the activity the
 * comment links to (a `commented` row, or the `updated` row of an item
 * update that carried the comment) and surface the name as
 * `comment.agent_name` (TASK-2760) — the same stamp, read through the row
 * the comment card stands in for on the timeline. Versions, items, structured note/decision
 * entries and SSE events record the actor KIND ("agent" / "user") and no
 * name; a surface holding one of those has nothing to render here and says
 * "agent" as it always did.
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
 *
 * WHICH MAKES IT A LEAF, NEVER A FRAGMENT. Because the actor authors this
 * value, any string a surface BUILDS around it hands the author influence
 * over the parts they did not write — a name can spell out the connective
 * text ("admin (via root)") or carry a bidi control that reorders whatever
 * was appended after it. So every surface renders it as its own isolated
 * element (<bdi>) and composes with siblings in markup rather than by
 * concatenation; the console audit log's `displayUser` is the worked
 * example, and returns parts for exactly this reason. That is not a
 * softening of the verbatim rule above: isolation alters no characters and
 * rejects no names, it just refuses to let one value redraw another.
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
	return agentNameValue(metadata?.agent);
}

/**
 * The name a comment carries directly (`comment.agent_name`, joined server-side
 * from its linked activity — see the file comment), or undefined when there is
 * none. Same contract as {@link agentNameOf}: a comment's name is the same
 * self-declared stamp, arriving as a field instead of inside a metadata blob.
 */
export function agentNameOfComment(comment: { agent_name?: unknown } | undefined | null): string | undefined {
	return agentNameValue(comment?.agent_name);
}

function agentNameValue(name: unknown): string | undefined {
	// A non-string cannot occur through agentMeta (it marshals map[string]string),
	// but this reads untrusted JSON off the wire, so it is checked rather than
	// assumed — an object here would otherwise render as "[object Object]".
	if (typeof name !== 'string' || name.length === 0) return undefined;
	return name;
}

/*
 * There is deliberately no `label(metadata, fallback)` wrapper. One existed
 * and had a single caller: every other site already holds the parsed
 * metadata and reaches for `agentNameOf`, and each supplies its own
 * fallback anyway — surfaces disagree on the nameless case ("agent" in the
 * feed's lowercase badges, "Agent" in the timeline's chips), so the wrapper
 * saved a `?? 'agent'` and cost an inconsistency in how five call sites
 * looked. Two functions, one difference between them: do you have the JSON
 * string or the parsed object.
 */
