import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createContentSaver } from './contentSaver.svelte';

describe('createContentSaver', () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});
	afterEach(() => {
		vi.useRealTimers();
	});

	it('debounces and coalesces rapid keystrokes into one save', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		saver.queue('a');
		vi.advanceTimersByTime(500);
		saver.queue('ab'); // supersedes 'a' before it fires
		vi.advanceTimersByTime(500);
		saver.queue('abc');

		// Nothing fires until a full debounce window elapses after the last edit.
		expect(save).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1200);

		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith('abc', { keepalive: false });
	});

	it('tracks the dirty flag: set on queue, clearable via clearPending', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		expect(saver.dirty).toBe(false);
		expect(saver.pending).toBeNull();

		saver.queue('hello');
		expect(saver.dirty).toBe(true);
		expect(saver.pending).toBe('hello');

		saver.clearPending();
		expect(saver.dirty).toBe(false);
		expect(saver.pending).toBeNull();
	});

	it('flushNow fires the pending save immediately and cancels the debounce', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		saver.queue('draft');
		const fired = saver.flushNow();

		expect(fired).toBe(true);
		expect(save).toHaveBeenCalledTimes(1);
		expect(save).toHaveBeenCalledWith('draft', { keepalive: false });

		// The queued debounce must NOT fire a second, racing save.
		vi.advanceTimersByTime(5000);
		expect(save).toHaveBeenCalledTimes(1);
	});

	it('plumbs the keepalive flag through flushNow (BUG-2024 unload path)', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		saver.queue('unsaved edit');
		const fired = saver.flushNow({ keepalive: true });

		expect(fired).toBe(true);
		expect(save).toHaveBeenCalledWith('unsaved edit', { keepalive: true });
	});

	it('does not save when clean', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		const fired = saver.flushNow({ keepalive: true });

		expect(fired).toBe(false);
		expect(save).not.toHaveBeenCalled();
	});

	it('cancel stops the debounce but leaves the dirty flag set', () => {
		const save = vi.fn();
		const saver = createContentSaver({ debounceMs: 1200, save });

		saver.queue('typing');
		saver.cancel();
		vi.advanceTimersByTime(5000);

		expect(save).not.toHaveBeenCalled();
		// Still dirty — cancel only stops the timer, it doesn't discard content.
		expect(saver.dirty).toBe(true);
		expect(saver.pending).toBe('typing');
	});

	it('uses the default debounce window when none is supplied', () => {
		const save = vi.fn();
		const saver = createContentSaver({ save });

		saver.queue('x');
		vi.advanceTimersByTime(1199);
		expect(save).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1);
		expect(save).toHaveBeenCalledTimes(1);
	});
});
