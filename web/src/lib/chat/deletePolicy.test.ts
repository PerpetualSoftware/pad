import { describe, expect, it } from 'vitest';
import { canDeleteMessage } from './deletePolicy';

// Mirrors internal/store/conversations.go:622 and
// internal/server/handlers_chat.go:314 handleDeleteChatMessage.
describe('direct conversation owner delete policy (TASK-4315)', () => {
	it.each([
		['direct human', { kind: 'direct' }, false, true, true, true],
		['direct agent', { kind: 'direct' }, false, false, true, true],
		['tombstone', { kind: 'direct' }, true, false, true, false],
		['group', { kind: 'group' }, false, true, true, false],
		['not loaded', null, false, true, true, false],
		['unverified', { kind: 'direct' }, false, true, false, false],
	] as const)('%s', (_name, conversation, deleted, mine, emailVerified, expected) => {
		expect(canDeleteMessage({ message: { deleted, mine }, conversation, emailVerified })).toBe(expected);
	});
});
