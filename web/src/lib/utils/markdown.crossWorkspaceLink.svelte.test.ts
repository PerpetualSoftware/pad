import { describe, it, expect } from 'vitest';
import { renderMarkdown } from './markdown';

// TASK-3119: `/-/r/{workspace}/{ref}` is a SERVER redirect with no client
// route behind it. A plain anchor let the SvelteKit router take the click and
// render it as [username]=- / [workspace]=r ("Workspace not found"), so the
// rendered cross-workspace wiki-link must carry rel="external", which the
// router leaves to the browser, through the sanitizer.
describe('cross-workspace wiki-link', () => {
	it('renders a resolver anchor the client router does not take', () => {
		const div = document.createElement('div');
		div.innerHTML = renderMarkdown('See [[other-ws::TASK-9]].', [], 'ws');
		const a = div.querySelector('a.cross-workspace');
		expect(a).toBeTruthy();
		expect(a!.getAttribute('href')).toBe('/-/r/other-ws/TASK-9');
		expect((a!.getAttribute('rel') ?? '').split(/\s+/)).toContain('external');
		// Same tab: nothing opens elsewhere.
		expect(a!.hasAttribute('target')).toBe(false);
	});
});
