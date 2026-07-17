import { describe, it, expect, beforeEach } from 'vitest';
import {
	pushEscapeHandler,
	runTopEscape,
	topEscapePriority,
	ESCAPE_PRIORITY,
	_resetEscapeStackForTests,
} from './escapeStack';

describe('escapeStack', () => {
	beforeEach(() => {
		_resetEscapeStackForTests();
	});

	it('returns false when the stack is empty', () => {
		expect(runTopEscape()).toBe(false);
	});

	it('invokes only the highest-priority handler per ESC (one layer)', () => {
		const calls: string[] = [];
		pushEscapeHandler(() => {
			calls.push('listFocus');
			return true;
		}, ESCAPE_PRIORITY.listFocus);
		pushEscapeHandler(() => {
			calls.push('pane');
			return true;
		}, ESCAPE_PRIORITY.pane);
		pushEscapeHandler(() => {
			calls.push('graph');
			return true;
		}, ESCAPE_PRIORITY.graphDrawer);

		expect(runTopEscape()).toBe(true);
		// Exactly one handler fired — the innermost (graph drawer).
		expect(calls).toEqual(['graph']);
	});

	it('closes one layer at a time, innermost-first, as layers unregister', () => {
		const calls: string[] = [];
		pushEscapeHandler(() => {
			calls.push('listFocus');
			return true;
		}, ESCAPE_PRIORITY.listFocus);
		const offPane = pushEscapeHandler(() => {
			calls.push('pane');
			return true;
		}, ESCAPE_PRIORITY.pane);
		const offGraph = pushEscapeHandler(() => {
			calls.push('graph');
			offGraph();
			return true;
		}, ESCAPE_PRIORITY.graphDrawer);

		// ESC #1: graph drawer (and it unregisters itself, mimicking close).
		runTopEscape();
		// ESC #2: pane (unregister it as the pane would on close).
		runTopEscape();
		offPane();
		// ESC #3: list focus clear.
		runTopEscape();

		expect(calls).toEqual(['graph', 'pane', 'listFocus']);
	});

	it('falls through to a lower-priority handler when the top declines', () => {
		const calls: string[] = [];
		// The list-focus layer is always registered but DECLINES when it has
		// nothing to clear (returns false), so ESC falls through.
		pushEscapeHandler(() => {
			calls.push('pane');
			return true;
		}, ESCAPE_PRIORITY.pane);
		pushEscapeHandler(() => {
			calls.push('graph-declines');
			return false;
		}, ESCAPE_PRIORITY.graphDrawer);

		expect(runTopEscape()).toBe(true);
		expect(calls).toEqual(['graph-declines', 'pane']);
	});

	it('returns false when every handler declines', () => {
		pushEscapeHandler(() => false, ESCAPE_PRIORITY.listFocus);
		expect(runTopEscape()).toBe(false);
	});

	it('unregister removes the handler', () => {
		const calls: string[] = [];
		const off = pushEscapeHandler(() => {
			calls.push('pane');
			return true;
		}, ESCAPE_PRIORITY.pane);
		off();
		expect(runTopEscape()).toBe(false);
		expect(calls).toEqual([]);
	});

	it('topEscapePriority reports the frontmost registered layer (or null when empty)', () => {
		expect(topEscapePriority()).toBeNull();

		const offPane = pushEscapeHandler(() => true, ESCAPE_PRIORITY.pane);
		pushEscapeHandler(() => true, ESCAPE_PRIORITY.listFocus);
		// Pane (20) outranks list-focus (10) → pane is frontmost.
		expect(topEscapePriority()).toBe(ESCAPE_PRIORITY.pane);

		// Opening the graph drawer (30) makes IT frontmost — the pane's two-level
		// ESC must defer while this is true (TASK-2122 Codex P2).
		const offGraph = pushEscapeHandler(() => true, ESCAPE_PRIORITY.graphDrawer);
		expect(topEscapePriority()).toBe(ESCAPE_PRIORITY.graphDrawer);

		// Closing the graph returns the pane to the front.
		offGraph();
		expect(topEscapePriority()).toBe(ESCAPE_PRIORITY.pane);

		offPane();
		expect(topEscapePriority()).toBe(ESCAPE_PRIORITY.listFocus);
	});

	it('breaks priority ties toward the most-recently registered handler', () => {
		const calls: string[] = [];
		pushEscapeHandler(() => {
			calls.push('first');
			return true;
		}, ESCAPE_PRIORITY.pane);
		pushEscapeHandler(() => {
			calls.push('second');
			return true;
		}, ESCAPE_PRIORITY.pane);

		runTopEscape();
		expect(calls).toEqual(['second']);
	});
});
