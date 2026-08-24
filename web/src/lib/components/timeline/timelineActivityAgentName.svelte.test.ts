import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineActivityCard from './TimelineActivityCard.svelte';
import type { Activity } from '$lib/types';

/**
 * TASK-2759. The card's actor chip said "Agent" for every agent write, so a
 * timeline could not tell you WHICH agent touched the item — the name has
 * been stamped on the activity's metadata since BUG-2542 and nothing read it.
 *
 * These assert the BINDING, not the helper (CONVE-19): `agentActor.ts` has
 * its own unit tests, and a correct helper that nothing calls here would pass
 * every one of them. So each case checks what the CARD renders, and the
 * load-bearing legs are the negative ones — a chip hardcoded back to "Agent"
 * still satisfies a name-is-somewhere assertion if the name also appears in
 * `actor_name`, which is why the two are asserted apart.
 */
function activity(overrides: Partial<Activity> = {}): Activity {
	return {
		id: 'act-1',
		workspace_id: 'ws-1',
		document_id: 'item-1',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		metadata: JSON.stringify({ agent: 'wren' }),
		created_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		...overrides
	} as Activity;
}

describe('TimelineActivityCard agent name', () => {
	it('renders the stamped name in place of the generic chip', () => {
		const { getByText, queryByText } = render(TimelineActivityCard, {
			activity: activity()
		});

		expect(getByText('wren')).toBeTruthy();
		// The counterfactual: the pre-TASK-2759 card rendered this, and would
		// still render it if the chip were rebuilt without reading metadata.
		expect(queryByText('Agent')).toBeNull();
	});

	it('renders a generic-looking client id verbatim', () => {
		// The retired GENERIC_AGENT_IDS shim swallowed exactly this value. A
		// reader seeing `claude-code` learns the writes came from one
		// undifferentiated client — a fact the filter hid behind "Agent".
		const { getByText, queryByText } = render(TimelineActivityCard, {
			activity: activity({ metadata: JSON.stringify({ agent: 'claude-code' }) })
		});

		expect(getByText('claude-code')).toBeTruthy();
		expect(queryByText('Agent')).toBeNull();
	});

	it('keeps the agent name and the human account separate', () => {
		// An agent write authenticates with a person's credentials, so
		// `actor_name` is that person. Both render; neither replaces nor
		// absorbs the other, because they are different facts.
		const { getByText } = render(TimelineActivityCard, {
			activity: activity({ actor_name: 'Dave' })
		});

		expect(getByText('wren')).toBeTruthy();
		expect(getByText('Dave')).toBeTruthy();
	});

	it.each([
		['no agent key', JSON.stringify({ changes: 'status: open → done' })],
		['an empty name', JSON.stringify({ agent: '' })],
		['a non-string name', JSON.stringify({ agent: 123 })],
		['unparseable metadata', 'not json']
	])('falls back to the generic chip given %s', (_case, metadata) => {
		const { getByText } = render(TimelineActivityCard, {
			activity: activity({ metadata })
		});

		expect(getByText('Agent')).toBeTruthy();
	});

	// Codex round 2. The name is attacker-influenced: it is whatever a client
	// put in X-Pad-Agent, and the server stores it without inspection. Svelte
	// text interpolation escapes it today, so this passes as written — the
	// point is that it would STOP passing if any of these render paths were
	// rewritten to `{@html}`, which is the plausible way an "allow rich agent
	// labels" change would arrive.
	it('renders a name containing markup as text, never as elements', () => {
		const payload = '<img src=x onerror="alert(1)">';
		const { container, getByText } = render(TimelineActivityCard, {
			activity: activity({ metadata: JSON.stringify({ agent: payload }) })
		});

		expect(container.querySelector('img')).toBeNull();
		expect(getByText(payload)).toBeTruthy();
	});

	// Codex round 11 — the element, not just the text. The chip's label sits
	// inline before the action label and timestamp, so a bidi control in a
	// self-declared name would reorder them if the label were not isolated.
	it('isolates the actor label so a bidi control cannot reorder the row', () => {
		const { container } = render(TimelineActivityCard, {
			activity: activity({ metadata: JSON.stringify({ agent: 'wren\u202egnimalb' }) })
		});

		const el = container.querySelector('.actor-label')!;
		expect(el.tagName).toBe('BDI');
		expect(el.textContent).toBe('wren\u202egnimalb');
	});

	it('never reads the stamp for a non-agent actor', () => {
		// Guards against keying the chip on metadata alone. A human's write can
		// carry an `agent` key — the merge in agentMeta is textual and this
		// blob is shared — and it is not a claim that an agent acted.
		const { getByText, queryByText } = render(TimelineActivityCard, {
			activity: activity({ actor: 'user', metadata: JSON.stringify({ agent: 'wren' }) })
		});

		expect(getByText('User')).toBeTruthy();
		expect(queryByText('wren')).toBeNull();
	});
});
