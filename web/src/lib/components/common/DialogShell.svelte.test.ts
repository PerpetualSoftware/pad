import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import DialogShell from './DialogShell.svelte';

const html = (text: string) => createRawSnippet(() => ({ render: () => text }));
function mount(extra: Record<string, unknown> = {}) {
	const onclose = vi.fn();
	render(DialogShell, { open: true, onclose, title: 'Forecast', subject: 'Rotate keys', subjectRef: 'TASK-14', mobile: false, children: html('<p>Body text</p>'),
		footer: html('<button class="btn primary">Save forecast</button>'), footerStart: html('<button class="btn ghost">Clear forecast</button>'), ...extra });
	return onclose;
}
function press(el: Element) {
	el.dispatchEvent(new Event('pointerdown', { bubbles: true }));
	el.dispatchEvent(new Event('pointerup', { bubbles: true }));
	el.dispatchEvent(new MouseEvent('click', { bubbles: true }));
}
afterEach(cleanup);

describe('DialogShell', () => {
	it('is a modal on desktop, named by its title, with the subject, body and both footer slots', () => {
		mount();
		const dialog = screen.getByRole('dialog', { name: 'Forecast' });
		expect(dialog.tagName).toBe('DIALOG');
		expect(screen.getAllByRole('heading', { level: 2 })).toHaveLength(1);
		expect(dialog.querySelector('.subject')).toHaveTextContent('Rotate keys TASK-14');
		expect(dialog).toHaveTextContent('Body text');
		const foot = dialog.querySelector('footer')!;
		expect([...foot.querySelectorAll('button')].map(button => button.textContent)).toEqual(['Clear forecast', 'Save forecast']);
		expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
	});
	it('is a bottom sheet on a phone, still named by its title, with a full-width footer', () => {
		mount({ mobile: true });
		const dialog = screen.getByRole('dialog', { name: 'Forecast' });
		expect(dialog.tagName).not.toBe('DIALOG'); expect(dialog).toHaveClass('bs-sheet');
		expect(screen.getAllByRole('heading', { level: 2 })).toHaveLength(1);
		expect(dialog.querySelector('.subject')).toHaveTextContent('Rotate keys TASK-14');
		expect(dialog.querySelector('footer')).toHaveClass('sheet');
	});
	it('renders no footer without a footer snippet', () => {
		mount({ footer: undefined, footerStart: undefined });
		expect(screen.getByRole('dialog').querySelector('footer')).toBeNull();
	});
	it('closes from Escape, the close button and the backdrop', async () => {
		const onclose = mount(); const dialog = screen.getByRole('dialog');
		await fireEvent(dialog, new Event('cancel', { cancelable: true }));
		await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
		press(dialog);
		expect(onclose).toHaveBeenCalledTimes(3);
	});
	it('ignores Escape, the close button and the backdrop while pending', async () => {
		const onclose = mount({ pending: true }); const dialog = screen.getByRole('dialog');
		await fireEvent(dialog, new Event('cancel', { cancelable: true }));
		await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
		press(dialog);
		expect(onclose).not.toHaveBeenCalled();
	});
	it('ignores Escape, the close button and the overlay on a phone while pending', async () => {
		const onclose = mount({ pending: true, mobile: true });
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
		await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
		press(document.querySelector('.bs-overlay')!);
		expect(onclose).not.toHaveBeenCalled();
	});
});
