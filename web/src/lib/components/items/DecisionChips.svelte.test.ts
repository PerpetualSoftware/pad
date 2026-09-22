import { describe, it, expect, vi, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { ItemDecision, ItemDecisionsResponse } from '$lib/types';

// DecisionChips (TASK-3118): renders nothing without current answers, and a
// response for an item the page has switched away from never paints.

const pending = new Map<string, (r: ItemDecisionsResponse) => void>();
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			decisions: vi.fn(
				(_ws: string, slug: string) =>
					new Promise<ItemDecisionsResponse>((resolve) => pending.set(slug, resolve))
			),
		},
	},
}));

// A real epoch behind a mock, so the fence the component captures is the one
// an identity change actually moves (BUG-3130).
const auth = vi.hoisted(() => {
	let epoch = 0;
	return {
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		changeIdentity() {
			epoch++;
		},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import DecisionChips from './DecisionChips.svelte';

function answer(key: string, noul: number): ItemDecision {
	return {
		id: key, item_id: 'x', question_set: 'attention', question_key: key, kind: 'noul',
		answer: { type: 'noul', noul }, confidence: null, provider: 'p', model: 'm',
		evaluated_at: '2026-09-22T00:00:00Z', current: true,
	};
}

async function settle() {
	await Promise.resolve();
	await Promise.resolve();
	flushSync();
}

let cmp: ReturnType<typeof mount> | null = null;
afterEach(() => {
	if (cmp) unmount(cmp);
	cmp = null;
	pending.clear();
	document.body.innerHTML = '';
});

describe('DecisionChips', () => {
	it('renders nothing when the item has no answers', async () => {
		cmp = mount(DecisionChips, { target: document.body, props: { wsSlug: 'ws', itemRef: 'a', itemId: 'A' } });
		flushSync();
		pending.get('a')!({ ref: 'A-1', decisions: [] });
		await settle();
		expect(document.querySelector('.decision-chips')).toBeNull();
	});

	it("does not paint a switched-away item's late response", async () => {
		const props = $state({ wsSlug: 'ws', itemRef: 'a', itemId: 'A' });
		cmp = mount(DecisionChips, { target: document.body, props });
		flushSync();
		props.itemRef = 'b';
		props.itemId = 'B';
		flushSync();
		pending.get('b')!({ ref: 'B-1', decisions: [answer('blocked', 0.8)] });
		await settle();
		// The counterfactual: A answers LAST, with a chip B does not have.
		pending.get('a')!({ ref: 'A-1', decisions: [answer('needs_human_decision', 0.95)] });
		await settle();
		const labels = [...document.querySelectorAll('.decision-chip-label')].map((e) => e.textContent);
		expect(labels).toEqual(['Blocked']);
		expect(document.querySelector('.decision-chip.flagged')).not.toBeNull();
	});

	it("does not paint a response issued under the previous identity (BUG-3130)", async () => {
		// Same item throughout: `latest` does not move, so only the identity
		// fence can refuse this. The answer describes what the PREVIOUS caller
		// may see.
		cmp = mount(DecisionChips, { target: document.body, props: { wsSlug: 'ws', itemRef: 'a', itemId: 'A' } });
		flushSync();
		auth.changeIdentity();
		pending.get('a')!({ ref: 'A-1', decisions: [answer('needs_human_decision', 0.95)] });
		await settle();
		expect(document.querySelector('.decision-chips')).toBeNull();
	});

	it('CONTROL: the same response paints when the identity holds still', async () => {
		// Without this the leg above passes for a component that never paints.
		cmp = mount(DecisionChips, { target: document.body, props: { wsSlug: 'ws', itemRef: 'a', itemId: 'A' } });
		flushSync();
		pending.get('a')!({ ref: 'A-1', decisions: [answer('needs_human_decision', 0.95)] });
		await settle();
		expect(document.querySelector('.decision-chips')).not.toBeNull();
	});
});
