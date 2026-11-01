// What the empty /messages page may claim (TASK-4186 PR B). Unknown is not none:
// "no one to message" is said only when both reads answered and found nothing.
import type { ChatConversationSummary, LiveSession } from '$lib/types';

// How an agent's name is resolved, in precedence order. Source of truth:
// internal/cli/agent_identity.go ResolveAgentName (registry row, .pad.toml
// agent_name, $PAD_AGENT, then runtime detection). The Go test
// TestAgentNameCopyMatchesResolverOrder pins this phrase in the docs too.
export const NAME_ORDER = 'registered name, then agent_name in .pad.toml, then PAD_AGENT';

// Under a harness other than Claude Code the session must be armed first (an
// unarmed monitor exits at once). Detail lives in docs/api.md, "Agent chat
// identity and arming"; keep this copy to the three conditions.
export const ARM_COMMAND = 'pad session arm';

export type ChatEmptyKind =
	| 'has-conversations'
	| 'unknown' // a read is pending, failed, or could not be trusted
	| 'start' // agents are listed, no conversation yet
	| 'no-agents';

export function chatEmptyKind(s: {
	loaded: boolean;
	failed: boolean;
	conversations: ChatConversationSummary[];
	sessions: LiveSession[] | 'unavailable' | null;
	rosterLength: number;
}): ChatEmptyKind {
	if (s.conversations.length > 0) return 'has-conversations';
	if (!s.loaded || s.failed) return 'unknown';
	if (s.rosterLength > 0) return 'start';
	// An empty roster is a fact only from a good sessions read.
	if (!Array.isArray(s.sessions)) return 'unknown';
	return 'no-agents';
}
