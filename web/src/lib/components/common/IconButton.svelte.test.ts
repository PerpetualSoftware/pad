// TASK-4515: an icon-only control keeps a name both by role and by text.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet, flushSync } from 'svelte';
import IconButton from './IconButton.svelte';
import ButtonBind from './fixtures/ButtonBind.fixture.svelte';

const glyph = createRawSnippet(() => ({ render: () => '<svg viewBox="0 0 20 20"></svg>' }));

afterEach(cleanup);

describe('IconButton', () => {
	it('is a button named by its label, findable by role and by text', async () => {
		const onclick = vi.fn();
		render(IconButton, { props: { label: 'Close Chat', onclick, children: glyph } });
		const b = screen.getByRole('button', { name: 'Close Chat' });
		expect(b.getAttribute('type')).toBe('button');
		expect(screen.getByText('Close Chat').classList.contains('sr-only')).toBe(true);
		expect(b.querySelector('svg')?.closest('[aria-hidden="true"]')).not.toBeNull();
		await fireEvent.click(b);
		expect(onclick).toHaveBeenCalledTimes(1);
	});

	it('renders a link when given href', () => {
		render(IconButton, { props: { label: 'All conversations', href: '/messages', children: glyph } });
		const a = screen.getByRole('link', { name: 'All conversations' });
		expect(a.getAttribute('href')).toBe('/messages');
		expect(screen.queryByRole('button')).toBeNull();
	});

	it('sizes: md is the default and grows on touch; sm does not unless asked', () => {
		const md = render(IconButton, { props: { label: 'a', children: glyph } }).container.querySelector('.icon-btn')!;
		expect([...md.classList]).toEqual(expect.arrayContaining(['md', 'touch']));
		cleanup();
		const sm = render(IconButton, { props: { label: 'b', size: 'sm', children: glyph } }).container.querySelector('.icon-btn')!;
		expect(sm.classList.contains('sm')).toBe(true);
		expect(sm.classList.contains('touch')).toBe(false);
		cleanup();
		const smTouch = render(IconButton, { props: { label: 'c', size: 'sm', touch: true, children: glyph } }).container.querySelector('.icon-btn')!;
		expect(smTouch.classList.contains('touch')).toBe(true);
	});

	it('binds the rendered element', () => {
		let got: HTMLElement | undefined;
		const { container } = render(ButtonBind, { props: { onbound: (_b, i) => (got = i) } });
		flushSync();
		expect(got).toBe(container.querySelector('.icon-btn'));
	});
});
