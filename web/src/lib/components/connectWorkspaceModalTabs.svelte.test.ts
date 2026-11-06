import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { getByRole } from '@testing-library/dom';
import { readFileSync } from 'node:fs';

vi.mock('$lib/api/client', () => ({
	api: { workspaces: { claimCode: vi.fn(() => new Promise(() => {})) } },
	PadApiError: class extends Error {}
}));

const { default: ConnectWorkspaceModal } = await import('./ConnectWorkspaceModal.svelte');
let root: HTMLElement;
let instance: ReturnType<typeof mount>;
afterEach(() => { if (instance) unmount(instance); root?.remove(); });

function render(mcpPublicUrl = 'https://pad.example.com/mcp', mcpAuth = ['oauth', 'pat']) {
	root = document.body.appendChild(document.createElement('div'));
	instance = mount(ConnectWorkspaceModal, {
		target: root,
		props: { open: true, workspaceSlug: 'ws', serverUrl: 'http://pad.lan:7777', mcpPublicUrl, mcpAuth }
	});
	flushSync();
}
function tabs(strip: Element) { return [...strip.querySelectorAll<HTMLButtonElement>('[role="tab"]')]; }
function selected(strip: Element, at: number) {
	const buttons = tabs(strip);
	expect(buttons.filter(b => b.tabIndex === 0)).toEqual([buttons[at]]);
	expect(buttons.filter(b => b.getAttribute('aria-selected') === 'true')).toEqual([buttons[at]]);
	for (const button of buttons) {
		const panel = document.getElementById(button.getAttribute('aria-controls') ?? '');
		expect(panel).toHaveAttribute('role', 'tabpanel');
		expect(panel).toHaveAttribute('aria-labelledby', buttons[at].id);
		expect(button.id).not.toBe('');
	}
}
function key(strip: Element, value: string, at: number) {
	document.activeElement!.dispatchEvent(new KeyboardEvent('keydown', { key: value, bubbles: true, cancelable: true }));
	flushSync();
	selected(strip, at);
	expect(document.activeElement).toBe(tabs(strip)[at]);
}
function exercise(strip: Element) {
	const buttons = tabs(strip);
	buttons[0].click();
	buttons[0].focus();
	flushSync();
	selected(strip, 0);
	key(strip, 'ArrowLeft', buttons.length - 1);
	key(strip, 'ArrowRight', 0);
	key(strip, 'ArrowRight', Math.min(1, buttons.length - 1));
	key(strip, 'Home', 0);
	key(strip, 'End', buttons.length - 1);
	key(strip, 'ArrowLeft', Math.max(0, buttons.length - 2));
}

describe('agent connection tabs (BUG-4336)', () => {
	it('names the close button', () => {
		render();
		expect(getByRole(root, 'button', { name: 'Close agent connection' })).toBeInTheDocument();
	});
	it.each([
		['OAuth', 'https://pad.example.com/mcp', ['oauth', 'pat'], 3],
		['PAT', 'http://pad.lan/mcp', ['pat'], 2],
		['CLI only', '', [], 1],
	] as const)('%s primary tabs select, focus, wrap and own their panel', (_, url, auth, count) => {
		render(url, [...auth]);
		const strip = root.querySelector('.primary-tabs')!;
		expect(tabs(strip)).toHaveLength(count);
		exercise(strip);
	});
	it('install tabs select, focus, wrap and own their panel', () => {
		render('', []);
		exercise(root.querySelector('.tab-strip')!);
	});
	it.each(['Enter', ' '])('%s remains native button activation on both strips', (value) => {
		render();
		for (const selector of ['.primary-tabs', '.tab-strip']) {
			const strip = root.querySelector(selector)!;
			const button = tabs(strip)[1]; // CLI, then Linux
			button.focus();
			expect(button.tagName).toBe('BUTTON');
			const event = new KeyboardEvent('keydown', { key: value, bubbles: true, cancelable: true });
			button.dispatchEvent(event);
			expect(event.defaultPrevented).toBe(false);
			// jsdom does not synthesize the browser's click default action.
			button.click();
			flushSync();
			selected(strip, 1);
			expect(document.activeElement).toBe(button);
		}
	});
	it.each(['altKey', 'ctrlKey', 'metaKey', 'shiftKey'] as const)('%s + an arrow key is left to the browser', (modifier) => {
		render();
		function untouched(strip: Element) {
			const buttons = tabs(strip);
			const at = buttons.findIndex(b => b.tabIndex === 0);
			expect(at).toBeGreaterThanOrEqual(0);
			buttons[at].focus();
			for (const keyName of ['ArrowRight', 'ArrowLeft', 'Home', 'End']) {
				const event = new KeyboardEvent('keydown', { key: keyName, bubbles: true, cancelable: true, [modifier]: true });
				document.activeElement!.dispatchEvent(event);
				expect(event.defaultPrevented).toBe(false);
				flushSync();
				selected(strip, at);
				expect(document.activeElement).toBe(buttons[at]);
			}
		}
		const primary = root.querySelector('.primary-tabs')!;
		// The install strip lives in the CLI panel, the second primary tab.
		tabs(primary)[1].click();
		flushSync();
		untouched(root.querySelector('[role="tablist"].tab-strip')!);
		untouched(primary);
	});
	it('names both tablists', () => {
		render();
		tabs(root.querySelector('.primary-tabs')!)[1].click();
		flushSync();
		expect(root.querySelector('.primary-tabs')).toHaveAccessibleName('Connection method');
		expect(root.querySelector('[role="tablist"].tab-strip')).toHaveAccessibleName('Install platform');
	});
	it('no tabpanel rule uses display: contents, which can drop the role from the accessibility tree', () => {
		// jsdom does not apply component CSS, so this reads the source the way the other style guards do.
		const source = readFileSync('src/lib/components/ConnectWorkspaceModal.svelte', 'utf8');
		const rules = [...source.matchAll(/([^{}]*\[role="tabpanel"\][^{}]*)\{([^}]*)\}/g)];
		expect(rules.length).toBeGreaterThan(0);
		for (const [, , declarations] of rules) expect(declarations).not.toMatch(/display:\s*contents/);
	});
});
