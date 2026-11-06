// BUG-4341: the module-level registries that let a late chat refusal find the
// composer that is on screen now, and let open() skip a send still in flight.
import { beforeEach, describe, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => ({ listeners: [] as Array<() => void> }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'u1',
		onIdentityChange(fn: () => void) {
			h.listeners.push(fn);
			return () => {};
		}
	}
}));

import { beginSend, clearComposerRegistry, deliverRefusal, endSend, isSending, registerComposer } from './composerRegistry';

beforeEach(() => clearComposerRegistry());

describe('composer registry', () => {
	it('hands a refusal to the composer registered for that user and conversation only', () => {
		const a = vi.fn();
		const b = vi.fn();
		registerComposer('u1', 1, a);
		registerComposer('u1', 2, b);
		expect(deliverRefusal('u1', 1, 'text', 'reply_deleted', null)).toBe(true);
		expect(a).toHaveBeenCalledWith('text', 'reply_deleted', null);
		expect(b).not.toHaveBeenCalled();
	});
	it('does not deliver across users or to an unregistered conversation', () => {
		const a = vi.fn();
		registerComposer('u1', 1, a);
		expect(deliverRefusal('u2', 1, 'x', 'reply_deleted', null)).toBe(false);
		expect(deliverRefusal('u1', 3, 'x', 'reply_deleted', null)).toBe(false);
		expect(a).not.toHaveBeenCalled();
	});
	it('unregistering removes the composer; a stale unregister leaves a newer one alone', () => {
		const old = vi.fn();
		const fresh = vi.fn();
		const dropOld = registerComposer('u1', 1, old);
		registerComposer('u1', 1, fresh);
		dropOld();
		expect(deliverRefusal('u1', 1, 'x', 'too_large', 4)).toBe(true);
		expect(fresh).toHaveBeenCalledWith('x', 'too_large', 4);
		expect(old).not.toHaveBeenCalled();
	});
	it('unregister of the live composer makes delivery fail', () => {
		const drop = registerComposer('u1', 1, vi.fn());
		drop();
		expect(deliverRefusal('u1', 1, 'x', 'reply_deleted', null)).toBe(false);
	});
	it('an identity change clears composers and in-flight keys', () => {
		registerComposer('u1', 1, vi.fn());
		beginSend('k');
		for (const fn of h.listeners) fn();
		expect(deliverRefusal('u1', 1, 'x', 'reply_deleted', null)).toBe(false);
		expect(isSending('k')).toBe(false);
	});
	it('tracks in-flight op keys by key, not by text', () => {
		expect(isSending('k1')).toBe(false);
		beginSend('k1');
		expect(isSending('k1')).toBe(true);
		expect(isSending('k2')).toBe(false);
		endSend('k1');
		expect(isSending('k1')).toBe(false);
	});
});
