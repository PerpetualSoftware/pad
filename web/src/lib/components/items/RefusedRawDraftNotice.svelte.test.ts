import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';

// BUG-3850 (lead ruling 0337bf77): a patch restore that must wait for the
// editor says why, keeps Restore disabled, and still offers Copy.
const { default: Notice } = await import('./RefusedRawDraftNotice.svelte');

function props(over: Record<string, unknown> = {}) {
	return { savedAt: 1_000, onrestore: vi.fn(), oncopy: vi.fn(), ondismiss: vi.fn(), newer: true, appliesChanges: true, ...over };
}

describe('RefusedRawDraftNotice: restore waiting for the editor', () => {
	it('blocked: the reason is shown, Restore is disabled, Copy leads and works', () => {
		const p = props({ restoreBlocked: 'Restore is available once the editor has connected. You can copy your text now.' });
		render(Notice, { props: p });
		expect(screen.getByTestId('refused-raw-draft-waiting').textContent).toContain('once the editor has connected');
		const restore = screen.getByRole('button', { name: 'Restore it' }) as HTMLButtonElement;
		expect(restore.disabled).toBe(true);
		restore.click();
		expect(p.onrestore).not.toHaveBeenCalled();
		const buttons = screen.getAllByRole('button').map((b) => b.textContent?.trim());
		expect(buttons.indexOf('Copy it')).toBeLessThan(buttons.indexOf('Restore it'));
		const copy = screen.getByRole('button', { name: 'Copy it' }) as HTMLButtonElement;
		expect(copy.disabled).toBe(false);
		copy.click();
		expect(p.oncopy).toHaveBeenCalledOnce();
	});

	it('not blocked: no reason, Restore enabled and leading', () => {
		const p = props();
		render(Notice, { props: p });
		expect(screen.queryByTestId('refused-raw-draft-waiting')).toBeNull();
		const restore = screen.getByRole('button', { name: 'Restore it' }) as HTMLButtonElement;
		expect(restore.disabled).toBe(false);
		const buttons = screen.getAllByRole('button').map((b) => b.textContent?.trim());
		expect(buttons.indexOf('Restore it')).toBeLessThan(buttons.indexOf('Copy it'));
		restore.click();
		expect(p.onrestore).toHaveBeenCalledOnce();
	});

	it('blocked with a conflict: only Copy, as before', () => {
		render(Notice, { props: props({ restoreBlocked: 'wait', conflict: { base: 'a', draft: 'b' } }) });
		expect(screen.queryByRole('button', { name: 'Restore it' })).toBeNull();
		expect(screen.queryByTestId('refused-raw-draft-waiting')).toBeNull();
		expect(screen.getByRole('button', { name: 'Copy it' })).toBeTruthy();
	});
});
