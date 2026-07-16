import { describe, it, expect } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import Parent from './Parent.svelte';

// Empirical probe for TASK-2117 / Codex Finding 5: on component unmount,
// does a top-level $effect's cleanup run BEFORE or AFTER a child
// component's onDestroy? ItemDetail's pane-close teardown flush lives in
// a top-level collab $effect cleanup and reads the child <Editor>'s
// markdown — so the answer determines whether the editor is still
// mounted when the flush fires.
describe('svelte 5 teardown order (top-level $effect cleanup vs child onDestroy)', () => {
	it('destroys the child (onDestroy) BEFORE the parent top-level $effect cleanup', () => {
		const order: string[] = [];
		const target = document.createElement('div');
		const app = mount(Parent, { target, props: { log: (s: string) => order.push(s) } });
		flushSync(); // let the deferred top-level $effect body run
		unmount(app);
		flushSync();
		// If this ever flips (a Svelte upgrade running top-level $effect
		// cleanups first), ItemDetail could read a live editor at teardown —
		// but the shadow-markdown fallback keeps the flush correct either way.
		expect(order).toEqual(['child-onDestroy', 'parent-effect-cleanup']);
	});
});
