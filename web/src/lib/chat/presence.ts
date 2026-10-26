// Agent presence roll-up for the chat roster (TASK-4044).

import type { LiveSession } from '$lib/types';

export type AgentPresence =
	| 'online'
	| 'busy_uncertain'
	| 'connected_not_accepting'
	| 'offline'
	| 'unknown';

const MAX_AGENT_NAME_RUNES = 64;

// Go's strings.TrimSpace trims unicode.IsSpace, which is exactly the White_Space
// property. JS String.trim() differs in one place: it also trims U+FEFF (BOM),
// which Go keeps. BUG-4092 (TASK-4044).
function trimSpace(s: string): string {
	return s.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
}

/**
 * The server's stored form of a self-declared agent name
 * (internal/server/agent_name.go::normalizeAgentName): trimmed, control
 * characters removed, at most 64 runes. Session agents and DM `agent_key`s both
 * went through it, so applying it again here only guards a mismatch.
 */
export function normalizeAgentName(raw: string): string {
	if (!raw) return '';
	let out = '';
	let n = 0;
	for (const c of trimSpace(raw)) {
		if (/\p{Cc}/u.test(c)) continue;
		if (n === MAX_AGENT_NAME_RUNES) break;
		out += c;
		n++;
	}
	return trimSpace(out);
}

/**
 * One presence state per agent. The roster is the union of the DM agent keys
 * and the distinct session agent names (empty names skipped). Precedence per
 * agent: any `online` (armed) session, else any `busy`, else any
 * `connected_not_accepting`, else `offline`. `'unavailable'` (the sessions
 * endpoint could not answer) maps every agent to `unknown`: "cannot tell" is
 * never rendered as offline.
 */
export function rollupPresence(
	conversationAgentKeys: readonly string[],
	sessions: readonly LiveSession[] | 'unavailable'
): Map<string, AgentPresence> {
	const out = new Map<string, AgentPresence>();
	const keys = conversationAgentKeys.map(normalizeAgentName).filter((k) => k !== '');
	if (sessions === 'unavailable') {
		for (const k of keys) out.set(k, 'unknown');
		return out;
	}
	const states = new Map<string, Set<LiveSession['state']>>();
	for (const s of sessions) {
		const name = normalizeAgentName(s.agent ?? '');
		if (name === '') continue;
		let set = states.get(name);
		if (!set) states.set(name, (set = new Set()));
		set.add(s.state);
	}
	for (const k of keys) out.set(k, 'offline');
	for (const [name, set] of states) {
		out.set(
			name,
			set.has('online')
				? 'online'
				: set.has('busy')
					? 'busy_uncertain'
					: set.has('connected_not_accepting')
						? 'connected_not_accepting'
						: 'offline'
		);
	}
	return out;
}
