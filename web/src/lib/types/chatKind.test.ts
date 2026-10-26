// TASK-4044 PR 3a (Q-W1): the wire `kind` is the server's two values (migration
// 158 CHECK), not a string. `npm run check` is the test: widening the type back
// to `string` turns the @ts-expect-error below into an "unused directive" error.
// (`*.svelte.test.ts` files are excluded from svelte-check, so this is a plain .test.ts.)
import { describe, expect, it } from 'vitest';
import type { ChatConversation } from './index';

describe('ChatConversation.kind (type pin)', () => {
	it('is direct | group', () => {
		const ok: ChatConversation['kind'][] = ['direct', 'group'];
		// @ts-expect-error 'dm' is not a kind the server writes
		const bad: ChatConversation['kind'] = 'dm';
		expect(ok).toHaveLength(2);
		expect(bad).toBe('dm');
	});
});
