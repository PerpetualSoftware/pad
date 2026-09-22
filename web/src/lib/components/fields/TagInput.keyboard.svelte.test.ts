// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3150 — tag suggestions are picked from the keyboard through the
// combobox pattern: focus stays on the input, the arrow keys move a highlight
// announced via aria-activedescendant, Enter adds the highlighted tag. The
// suggestions themselves stay out of the tab order (BUG-3148).
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import TagInput from './TagInput.svelte';

const SUGGESTIONS = ['alpha', 'alpine', 'beta'];

function setup(tags: string[] = []) {
	const onchange = vi.fn();
	render(TagInput, { props: { tags, onchange, suggestions: SUGGESTIONS } });
	const input = document.querySelector<HTMLInputElement>('input.tag-entry')!;
	return { input, onchange };
}

function highlighted(): string | null {
	return document.querySelector('.tag-suggestion[aria-selected="true"]')?.textContent?.trim() ?? null;
}

async function key(input: HTMLInputElement, k: string) {
	await fireEvent.keyDown(input, { key: k });
	await tick();
}

beforeEach(() => cleanup());

describe('TagInput keyboard selection (BUG-3150)', () => {
	it('ArrowDown highlights from the input; Enter adds THAT tag, not the typed text', async () => {
		const { input, onchange } = setup();
		await fireEvent.focus(input);
		await fireEvent.input(input, { target: { value: 'al' } });
		await key(input, 'ArrowDown');
		expect(highlighted()).toBe('alpha');
		// Announced from the input, which keeps focus.
		const active = document.querySelector('.tag-suggestion[aria-selected="true"]')!;
		expect(input.getAttribute('aria-activedescendant')).toBe(active.id);
		expect(input.getAttribute('role')).toBe('combobox');
		await key(input, 'Enter');
		expect(onchange).toHaveBeenCalledWith(['alpha']);
	});

	it('clamps at the end, and ArrowUp past the top clears the highlight (Enter then adds the typed text)', async () => {
		const { input, onchange } = setup();
		await fireEvent.focus(input);
		await fireEvent.input(input, { target: { value: 'al' } });
		await key(input, 'ArrowDown');
		await key(input, 'ArrowDown');
		await key(input, 'ArrowDown');
		expect(highlighted()).toBe('alpine');
		await key(input, 'ArrowUp');
		await key(input, 'ArrowUp');
		expect(highlighted()).toBeNull();
		expect(input.getAttribute('aria-activedescendant')).toBeNull();
		await key(input, 'Enter');
		expect(onchange).toHaveBeenCalledWith(['al']);
	});

	it('the highlight follows the TAG when typing re-filters the list, and drops when it is filtered out', async () => {
		const { input } = setup();
		await fireEvent.focus(input);
		await fireEvent.input(input, { target: { value: 'a' } });
		await key(input, 'ArrowDown');
		await key(input, 'ArrowDown');
		expect(highlighted()).toBe('alpine');
		// 'alpi' removes 'alpha' ABOVE it: an index-held highlight would now sit
		// on whatever took slot 1 (nothing), or on the wrong tag.
		await fireEvent.input(input, { target: { value: 'alpi' } });
		await tick();
		expect(highlighted()).toBe('alpine');
		await fireEvent.input(input, { target: { value: 'b' } });
		await tick();
		expect(highlighted()).toBeNull();
	});

	it('Escape closes the list and clears the highlight', async () => {
		const { input } = setup();
		await fireEvent.focus(input);
		await key(input, 'ArrowDown');
		expect(highlighted()).toBe('alpha');
		await key(input, 'Escape');
		expect(document.querySelector('.tag-suggestions')).toBeNull();
		expect(input.getAttribute('aria-expanded')).toBe('false');
	});

	it('the suggestions stay out of the tab order (BUG-3148)', async () => {
		const { input } = setup();
		await fireEvent.focus(input);
		await tick();
		const opts = [...document.querySelectorAll<HTMLElement>('.tag-suggestion')];
		expect(opts.length).toBeGreaterThan(0);
		expect(opts.every((o) => o.tabIndex === -1)).toBe(true);
	});
});
