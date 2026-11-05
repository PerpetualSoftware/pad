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
// One module-level regex, no g or y flag so `.test` carries no lastIndex state (r6fix R-L4).
const WHITE_SPACE = /\p{White_Space}/u;

export function trimSpace(s: string): string {
	// An index scan from both ends: the old `^ws+|ws+$` regex was quadratic on an
	// interior whitespace run, and the composer runs this per keystroke (r6 B2-M1).
	const isWs = (c: string) => WHITE_SPACE.test(c);
	const cps = Array.from(s);
	let lo = 0;
	let hi = cps.length;
	while (lo < hi && isWs(cps[lo])) lo++;
	while (hi > lo && isWs(cps[hi - 1])) hi--;
	return cps.slice(lo, hi).join('');
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
 * One presence state per agent (DOC-4316 section 7). The roster is the union of
 * the DM agent keys and the distinct session agent names (empty names skipped).
 * Only an ARMED session can receive a chat notice, and the server reports `busy`
 * over `armed` (handlers_sessions.go), so a busy session may be unarmed. Per agent:
 * an armed session that is not busy is `online`; else armed sessions that are all
 * busy read `busy_uncertain`; else any connected session is
 * `connected_not_accepting` (an unarmed busy one included); else `offline`.
 * `'unavailable'` (the sessions endpoint could not answer) maps every agent to
 * `unknown`: "cannot tell" is never rendered as offline.
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
	const byAgent = new Map<string, LiveSession[]>();
	for (const s of sessions) {
		const name = normalizeAgentName(s.agent ?? '');
		if (name === '') continue;
		let list = byAgent.get(name);
		if (!list) byAgent.set(name, (list = []));
		list.push(s);
	}
	for (const k of keys) out.set(k, 'offline');
	for (const [name, list] of byAgent) {
		const armed = list.filter((s) => s.armed);
		out.set(
			name,
			armed.some((s) => s.state !== 'busy')
				? 'online'
				: armed.length > 0
					? 'busy_uncertain'
					: 'connected_not_accepting'
		);
	}
	return out;
}

/** How many ARMED sessions each named agent has, for the "2 armed sessions" detail. Agents with none are absent. */
export function armedSessionCounts(sessions: readonly LiveSession[]): Map<string, number> {
	const out = new Map<string, number>();
	for (const s of sessions) {
		if (!s.armed) continue;
		const name = normalizeAgentName(s.agent ?? '');
		if (name === '') continue;
		out.set(name, (out.get(name) ?? 0) + 1);
	}
	return out;
}
