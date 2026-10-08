import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';

// TASK-2199: the notice that hands back offline edits a reload discarded.
// Copy puts the text on the clipboard and only then lets the caller drop the
// notice; a failed copy keeps it.
const clip = vi.hoisted(() => ({ ok: true, copied: [] as string[] }));
vi.mock('$lib/utils/clipboard', () => ({
	copyToClipboard: async (t: string) => {
		clip.copied.push(t);
		return clip.ok;
	}
}));

const { default: Notice } = await import('./OfflineRecoveryNotice.svelte');

beforeEach(() => {
	clip.ok = true;
	clip.copied.length = 0;
});

describe('OfflineRecoveryNotice', () => {
	it('shows the version and copies it, then reports copied', async () => {
		const oncopied = vi.fn();
		render(Notice, { props: { text: 'my offline version', oncopied, ondismiss: vi.fn() } });
		expect((screen.getByLabelText('Your offline version') as HTMLTextAreaElement).value).toBe('my offline version');
		screen.getByRole('button', { name: 'Copy your version' }).click();
		await waitFor(() => expect(oncopied).toHaveBeenCalledWith('my offline version'));
		expect(clip.copied).toEqual(['my offline version']);
	});

	it('a failed copy does not report copied', async () => {
		clip.ok = false;
		const oncopied = vi.fn();
		const oncopyfailed = vi.fn();
		render(Notice, { props: { text: 't', oncopied, oncopyfailed, ondismiss: vi.fn() } });
		screen.getByRole('button', { name: 'Copy your version' }).click();
		await waitFor(() => expect(oncopyfailed).toHaveBeenCalledTimes(1));
		expect(oncopied).not.toHaveBeenCalled();
	});

	it('Dismiss reports dismissed', () => {
		const ondismiss = vi.fn();
		render(Notice, { props: { text: 't', oncopied: vi.fn(), ondismiss } });
		screen.getByRole('button', { name: 'Dismiss' }).click();
		expect(ondismiss).toHaveBeenCalledTimes(1);
	});
});
