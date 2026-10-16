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

// TASK-3856: what the notice says matches what Restore would do, per state.
describe('RefusedRawDraftNotice: the wording and buttons match what Restore does', () => {
	const names = () => screen.getAllByRole('button').map((b) => b.textContent?.trim());

	it('stale base, seq moved: says the changes cannot be applied, offers only Copy', () => {
		const p = props({ baseStale: true });
		render(Notice, { props: p });
		expect(screen.getByTestId('refused-raw-draft-stale').textContent).toContain("can't be applied by themselves");
		expect(screen.queryByText(/restoring applies only your changes/)).toBeNull();
		expect(screen.queryByTestId('refused-raw-draft-conflict')).toBeNull();
		expect(names()).toEqual(['Copy it', 'Discard']);
		screen.getByRole('button', { name: 'Copy it' }).click();
		expect(p.oncopy).toHaveBeenCalledOnce();
	});

	it('stale base while the editor is not ready: the wait reason is not shown, only Copy', () => {
		render(Notice, { props: props({ baseStale: true, restoreBlocked: 'wait for the editor' }) });
		expect(screen.getByTestId('refused-raw-draft-stale')).toBeTruthy();
		expect(screen.queryByTestId('refused-raw-draft-waiting')).toBeNull();
		expect(names()).toEqual(['Copy it', 'Discard']);
	});

	it('not stale, seq moved: promises a patch, Restore leads', () => {
		render(Notice, { props: props() });
		expect(screen.getByText(/restoring applies only your changes and keeps the newer version/)).toBeTruthy();
		expect(screen.queryByTestId('refused-raw-draft-stale')).toBeNull();
		expect(names()).toEqual(['Restore it', 'Copy it', 'Discard']);
	});

	it('seq unmoved (whole-body restore): a stale flag changes nothing, Restore leads', () => {
		render(Notice, { props: props({ newer: false, appliesChanges: false, baseStale: true }) });
		expect(screen.queryByTestId('refused-raw-draft-stale')).toBeNull();
		expect(screen.queryByText(/This item changed/)).toBeNull();
		expect(names()).toEqual(['Restore it', 'Copy it', 'Discard']);
	});

	it('a conflict shown takes precedence over the stale wording', () => {
		render(Notice, { props: props({ baseStale: true, conflict: { base: 'a', draft: 'b' } }) });
		expect(screen.getByTestId('refused-raw-draft-conflict')).toBeTruthy();
		expect(screen.queryByTestId('refused-raw-draft-stale')).toBeNull();
		expect(names()).toEqual(['Copy it', 'Discard']);
	});
});
