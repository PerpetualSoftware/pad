import type { ChatConversation, ChatMessage } from '$lib/types';

// Mirrors internal/store/conversations.go:622 and
// internal/server/handlers_chat.go:314 handleDeleteChatMessage: the server refuses
// msg.AuthorUserID != actor.UserID || (actor.AgentKey != "" && actor.AgentKey != msg.AuthorAgentKey).
// Cookie sessions carry no AgentKey (handlers_chat.go:40-42), so a direct
// conversation's owner may delete both human and agent messages: mine=false
// does not prohibit deletion. The wire message has no author_user_id; ownership
// in a group cannot be mirrored here, so hide Delete conservatively.
export function canDeleteMessage({ message, conversation, emailVerified }: {
	message: Pick<ChatMessage, 'deleted' | 'mine'>;
	conversation: Pick<ChatConversation, 'kind'> | null | undefined;
	emailVerified: boolean;
}): boolean {
	return conversation?.kind === 'direct' && !message.deleted && emailVerified;
}
