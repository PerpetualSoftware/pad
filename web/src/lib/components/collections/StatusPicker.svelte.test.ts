/**
 * BUG-3157 — the status chip is a picker, not a one-tap cycle. On a board
 * grouped by status the old cycle moved a card a lane on a single tap meant to
 * open it (reproduced on mobile by web/e2e/bug-3157-*.spec.ts). These legs pin
 * the component's own contract; the views' tests pin that every surface uses it.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import StatusPicker from './StatusPicker.svelte';
import { STATUS_CHIP, chooseStatus, openStatusPicker, rowLabel, statusChipIn, statusRows } from './statusPickerTestKit';

afterEach(() => cleanup());

const OPTIONS = ['open', 'in_progress', 'done'];

function renderPicker(value = 'open') {
	const onselect = vi.fn();
	const screen = render(StatusPicker, { props: { value, options: OPTIONS, onselect } });
	return { screen, onselect };
}

describe('StatusPicker', () => {
	it('a click opens the picker and writes NOTHING — the defect was a write on the click', async () => {
		const { screen, onselect } = renderPicker();
		const rows = await openStatusPicker(screen.container);
		expect(onselect).not.toHaveBeenCalled();
		expect(rows.map(rowLabel)).toEqual(['Open', 'In Progress', 'Done']);
		expect(statusChipIn(screen.container).getAttribute('aria-expanded')).toBe('true');
	});

	it('choosing a row writes exactly that status, once, and closes', async () => {
		const { screen, onselect } = renderPicker();
		await chooseStatus(screen.container, 'done');
		expect(onselect).toHaveBeenCalledTimes(1);
		expect(onselect).toHaveBeenCalledWith('done');
		expect(statusRows()).toHaveLength(0);
		expect(statusChipIn(screen.container).getAttribute('aria-expanded')).toBe('false');
	});

	it('choosing the current status writes nothing', async () => {
		const { screen, onselect } = renderPicker('in_progress');
		await chooseStatus(screen.container, 'in_progress');
		expect(onselect).not.toHaveBeenCalled();
	});

	it('checks the current status and only that', async () => {
		const { screen } = renderPicker('in_progress');
		const rows = await openStatusPicker(screen.container);
		expect(rows.filter((r) => r.getAttribute('aria-checked') === 'true').map(rowLabel)).toEqual(['In Progress']);
	});

	it('renders the menu OUTSIDE the chip’s container', async () => {
		// The chip lives inside a card's <a> and the board's dnd zone. Rows
		// rendered there would sit inside a link, and svelte-dnd-action rewrites
		// the roles of what its zone contains.
		const { screen } = renderPicker();
		await openStatusPicker(screen.container);
		expect(screen.container.querySelector('[role="menuitemradio"]')).toBeNull();
		expect(statusRows().length).toBe(OPTIONS.length);
	});

	it('a second click on the chip closes it without writing', async () => {
		const { screen, onselect } = renderPicker();
		await openStatusPicker(screen.container);
		await fireEvent.click(statusChipIn(screen.container));
		expect(statusRows()).toHaveLength(0);
		expect(onselect).not.toHaveBeenCalled();
	});

	it('is a menu trigger by its ARIA, which is also what the views’ tests find it by', () => {
		const { screen } = renderPicker();
		expect(screen.container.querySelector(STATUS_CHIP)).not.toBeNull();
	});
});
