import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineCommentCard from './TimelineCommentCard.svelte';
import type { Comment } from '$lib/types';

/**
 * TASK-2760. An agent's comment rendered under a generic "Agent" chip next
 * to the HUMAN's name, so a thread could not tell you which agent said what.
 * The server now joins the name onto the comment (`comment.agent_name`) and
 * the card renders it in the chip.
 *
 * These assert the BINDING, not the helper (CONVE-19): each checks what the
 * CARD renders, and the load-bearing legs are the negative ones — a chip
 * hardcoded back to "Agent" still satisfies a name-is-somewhere assertion if
 * the name also appears in `author`, which is why the two are asserted apart.
 * Same shape as timelineActivityAgentName.svelte.test.ts, deliberately.
 */
function comment(overrides: Partial<Comment> = {}): Comment {
	return {
		id: 'c-1',
		item_id: 'item-1',
		workspace_id: 'ws-1',
		author: 'Dave',
		body: 'hello',
		created_by: 'agent',
		source: 'cli',
		agent_name: 'wren',
		created_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		updated_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		...overrides
	};
}

const noop = () => {};

function renderCard(c: Comment) {
	return render(TimelineCommentCard, {
		comment: c,
		wsSlug: 'ws',
		items: [],
		onDelete: noop,
		onReply: noop,
		onEdit: noop,
		onReaction: noop,
		onRemoveReaction: noop
	});
}

describe('TimelineCommentCard agent name', () => {
	it('renders the declared name in place of the generic chip', () => {
		const { getByText, queryByText } = renderCard(comment());

		expect(getByText('wren')).toBeTruthy();
		// The counterfactual: the pre-TASK-2760 card rendered this, and would
		// still render it if the chip were rebuilt without reading the field.
		expect(queryByText('Agent')).toBeNull();
	});

	it('renders a generic-looking client id verbatim', () => {
		// A value the retired display filter swallowed; and the badge's
		// uppercase transform is switched off for a named agent, so the DOM
		// text a reader copies is the value the client sent.
		const { getByText, queryByText, container } = renderCard(
			comment({ agent_name: 'claude-code' })
		);

		expect(getByText('claude-code')).toBeTruthy();
		expect(queryByText('Agent')).toBeNull();
		expect(container.querySelector('.author-badge.author-named')).not.toBeNull();
	});

	it('keeps the agent name and the human account separate', () => {
		// An agent's comment authenticates with a person's credentials, so
		// `author` is that person. Both render; neither replaces nor absorbs
		// the other, because they are different facts.
		const { getByText } = renderCard(comment({ author: 'Dave' }));

		expect(getByText('wren')).toBeTruthy();
		expect(getByText('Dave')).toBeTruthy();
	});

	it.each([
		['no name', undefined],
		['an empty name', ''],
		['a non-string name', 123 as unknown as string]
	])('falls back to the generic chip given %s', (_case, agent_name) => {
		const { getByText, container } = renderCard(comment({ agent_name }));

		expect(getByText('Agent')).toBeTruthy();
		expect(container.querySelector('.author-named')).toBeNull();
	});

	// The name is attacker-influenced: it is whatever a client put in
	// X-Pad-Agent, and the server stores it without inspection. Svelte text
	// interpolation escapes it today — the point is that this would STOP
	// passing if the render path were rewritten to `{@html}`.
	it('renders a name containing markup as text, never as elements', () => {
		const payload = '<img src=x onerror="alert(1)">';
		const { container, getByText } = renderCard(comment({ agent_name: payload }));

		expect(container.querySelector('img')).toBeNull();
		expect(getByText(payload)).toBeTruthy();
	});

	// The element, not just the text: the chip sits inline before the author,
	// source and timestamp, so a bidi control in a self-declared name would
	// reorder them if the label were not isolated.
	it('isolates the actor label so a bidi control cannot reorder the row', () => {
		const { container } = renderCard(comment({ agent_name: 'wren\u202egnimalb' }));

		const el = container.querySelector('.actor-label')!;
		expect(el.tagName).toBe('BDI');
		expect(el.textContent).toBe('wren\u202egnimalb');
	});

	it('never reads the name for a non-agent actor', () => {
		// Guards against keying the chip on the field alone. The server
		// populates agent_name from the linked activity whatever the actor
		// kind is; it is the KIND that says an agent acted.
		const { getByText, queryByText } = renderCard(
			comment({ created_by: 'user', agent_name: 'wren' })
		);

		expect(getByText('User')).toBeTruthy();
		expect(queryByText('wren')).toBeNull();
	});

	it('renders a nested reply through the same path', () => {
		// Replies are nested Comment objects, not timeline entries — the
		// reason the field lives on the comment. A reply by a different agent
		// must show ITS name, and the parent's chip must not leak into it.
		const { getByText, queryByText } = renderCard(
			comment({
				created_by: 'user',
				agent_name: undefined,
				replies: [
					comment({ id: 'c-2', parent_id: 'c-1', body: 'reply', agent_name: 'rook' })
				]
			})
		);

		expect(getByText('rook')).toBeTruthy();
		expect(getByText('User')).toBeTruthy();
		expect(queryByText('Agent')).toBeNull();
	});
});
