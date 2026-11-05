// TASK-2221 (audit C39): the dialog renders the store's question and answers it.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { onIdentityChange: () => () => {} } }));

const { confirmDialog } = await import('$lib/stores/confirmDialog.svelte');
const { default: ConfirmDialog } = await import('./ConfirmDialog.svelte');

let app: Record<string, unknown> | null = null;
let host: HTMLElement;

async function settle() {
	for (let i = 0; i < 4; i++) {
		await tick();
		flushSync();
	}
}

function open() {
	host = document.createElement('div');
	document.body.appendChild(host);
	app = mount(ConfirmDialog, { target: host }) as Record<string, unknown>;
}

const buttons = () => [...document.body.querySelectorAll<HTMLButtonElement>('button')];
const byText = (t: string) => buttons().find((b) => b.textContent?.trim() === t);

afterEach(() => {
	confirmDialog.abandonAll();
	if (app) unmount(app);
	app = null;
	host?.remove();
});

describe('ConfirmDialog', () => {
	it('shows the title, message and confirm label; the confirm button answers yes', async () => {
		open();
		const answer = confirmDialog.request({ title: 'Delete this comment?', message: 'Gone for good.', confirmLabel: 'Delete', danger: true });
		await settle();
		expect(document.body.textContent).toContain('Delete this comment?');
		expect(document.body.textContent).toContain('Gone for good.');
		byText('Delete')!.click();
		expect(await answer).toBe(true);
	});

	it('focusCancel explicitly focuses Cancel on open', async () => {
		open();
		void confirmDialog.request({ title: 'Delete?', message: 'Gone.', confirmLabel: 'Delete', focusCancel: true });
		await settle();
		expect(document.activeElement).toBe(byText('Cancel'));
	});

	it('focusCancel resets focus for a queued question in an already-open dialog', async () => {
		open();
		void confirmDialog.request({ title: 'First', message: 'First', confirmLabel: 'Proceed' });
		void confirmDialog.request({ title: 'Delete?', message: 'Gone.', confirmLabel: 'Delete', focusCancel: true });
		await settle();
		byText('Proceed')!.focus();
		byText('Proceed')!.click();
		await settle();
		expect(document.activeElement).toBe(byText('Cancel'));
	});

	it('without focusCancel keeps the existing native autofocus behavior', async () => {
		open();
		void confirmDialog.request({ title: 'T', message: 'M', confirmLabel: 'Proceed' });
		await settle();
		expect(byText('Cancel')!.hasAttribute('autofocus')).toBe(true);
		expect(byText('Proceed')!.hasAttribute('autofocus')).toBe(false);
	});

	it('Cancel answers no', async () => {
		open();
		const answer = confirmDialog.request({ title: 'T', message: 'M', confirmLabel: 'Replace content' });
		await settle();
		byText('Cancel')!.click();
		expect(await answer).toBe(false);
	});

	it('a named cancel label replaces Cancel and still answers no (TASK-3543)', async () => {
		open();
		const answer = confirmDialog.request({ title: 'T', message: 'M', confirmLabel: 'Cancel invitation', cancelLabel: 'Keep invitation' });
		await settle();
		expect(byText('Cancel')).toBeUndefined();
		byText('Keep invitation')!.click();
		expect(await answer).toBe(false);
	});

	it('renders nothing while no question is open', async () => {
		open();
		await settle();
		expect(byText('Cancel')).toBeUndefined();
	});
});
