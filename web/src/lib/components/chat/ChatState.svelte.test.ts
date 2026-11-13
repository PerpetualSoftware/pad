// TASK-4515: the one centred state (off, no agents, empty thread, not found, failed).
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import ChatState from './ChatState.svelte';

const html = (s: string) => createRawSnippet(() => ({ render: () => s }));

afterEach(cleanup);

describe('ChatState', () => {
	it('renders the heading at the level asked for, the body and the actions', () => {
		const { container } = render(ChatState, {
			props: {
				heading: 'Chat is off',
				level: 1,
				children: html('<p>Body text</p>'),
				actions: html('<button class="act">Turn on Chat</button>')
			}
		});
		expect(screen.getByRole('heading', { level: 1, name: 'Chat is off' })).toBeTruthy();
		expect(container.querySelector('.chat-state p')?.textContent).toBe('Body text');
		expect(container.querySelector('.chat-state .actions .act')).not.toBeNull();
	});

	it('level 2 renders an h2', () => {
		render(ChatState, { props: { heading: 'No one to message yet', level: 2 } });
		expect(screen.getByRole('heading', { level: 2, name: 'No one to message yet' })).toBeTruthy();
	});

	it('without a level the title is not a heading', () => {
		const { container } = render(ChatState, { props: { heading: 'No messages yet' } });
		expect(screen.queryByRole('heading')).toBeNull();
		expect(container.querySelector('.chat-state .title')?.textContent).toBe('No messages yet');
	});

	it('draws a decorative icon tile in the asked tone', () => {
		const { container } = render(ChatState, { props: { heading: 'x', tone: 'accent' } });
		const tile = container.querySelector('.tile')!;
		expect(tile.getAttribute('aria-hidden')).toBe('true');
		expect(tile.classList.contains('accent')).toBe(true);
		cleanup();
		const muted = render(ChatState, { props: { heading: 'y' } }).container.querySelector('.tile')!;
		expect(muted.classList.contains('muted')).toBe(true);
	});

	it('has no actions row when none are given', () => {
		const { container } = render(ChatState, { props: { heading: 'x' } });
		expect(container.querySelector('.actions')).toBeNull();
	});
});
