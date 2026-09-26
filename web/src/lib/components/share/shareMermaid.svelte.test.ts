// TASK-2248 U2 (audit C120): ```mermaid blocks on a public share draw as
// diagrams, and fall back to their code when they cannot be drawn.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';

const mm = vi.hoisted(() => ({
	initialize: vi.fn(),
	render: vi.fn(async (_id: string, source: string) => {
		if (source.includes('INVALID')) throw new Error('parse error');
		if (source.includes('HANG')) return new Promise<never>(() => {});
		return { svg: `<svg data-src="${source.trim().split('\n')[0]}"></svg>` };
	}),
}));
vi.mock('mermaid', () => ({ default: mm }));

import { renderMermaidBlocks } from './shareMermaid';
import PublicItemExpansion from './PublicItemExpansion.svelte';

const block = (src: string) => `<pre><code class="language-mermaid">${src}</code></pre>`;
function host(html: string): HTMLElement {
	const d = document.createElement('div');
	d.innerHTML = html;
	document.body.appendChild(d);
	return d;
}

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('share mermaid (C120)', () => {
	it('a valid block draws, and its code is hidden rather than removed', async () => {
		const d = host(block('graph TD\nA-->B'));
		renderMermaidBlocks(d);
		await vi.waitFor(() => expect(d.querySelector('.share-mermaid svg')).not.toBeNull());
		expect(d.querySelector('.share-mermaid svg')!.getAttribute('data-src')).toBe('graph TD');
		expect(d.querySelector('pre')!.hidden).toBe(true);
	});

	it('an unparseable block shows its code again, with no diagram left behind', async () => {
		const d = host(block('INVALID graph'));
		renderMermaidBlocks(d);
		await vi.waitFor(() => expect(d.querySelector('.share-mermaid')).toBeNull());
		expect(d.querySelector('pre')!.hidden).toBe(false);
		expect(d.textContent).toContain('INVALID graph');
	});

	it('a second pass over the same body draws nothing twice', async () => {
		const d = host(block('graph LR\nX-->Y'));
		renderMermaidBlocks(d);
		renderMermaidBlocks(d);
		await vi.waitFor(() => expect(d.querySelector('.share-mermaid svg')).not.toBeNull());
		expect(d.querySelectorAll('.share-mermaid')).toHaveLength(1);
	});

	it('other code blocks are left alone', async () => {
		const d = host('<pre><code class="language-js">graph TD</code></pre>');
		renderMermaidBlocks(d);
		await Promise.resolve();
		expect(d.querySelector('.share-mermaid')).toBeNull();
		expect(d.querySelector('pre')!.hidden).toBe(false);
	});

	it('the collection inline-expand draws its body too (the binding, not just the function)', async () => {
		const { container } = render(PublicItemExpansion, {
			item: { key: 'k', title: 'T', ref: 'TASK-1', fields: {}, content: 'x', contentStale: false },
			fields: [],
			html: block('graph TD\nP-->Q'),
		});
		await vi.waitFor(() => expect(container.querySelector('.expansion-content .share-mermaid svg')).not.toBeNull());
	});

	// Codex round 1: the code used to be hidden up front and restored only on a
	// rejection, so a render that never settled left the reader nothing.
	// LAST IN THE FILE, deliberately: the render queue is one module-level
	// promise chain, so a render that never settles stalls every render queued
	// after it. That is true of the editor too, and is filed separately.
	it('a diagram that never finishes drawing leaves the code on screen', async () => {
		const d = host(block('HANG graph'));
		renderMermaidBlocks(d);
		await vi.waitFor(() => expect(mm.render).toHaveBeenCalledWith(expect.any(String), 'HANG graph'));
		expect(d.querySelector('pre')!.hidden).toBe(false);
	});
});
