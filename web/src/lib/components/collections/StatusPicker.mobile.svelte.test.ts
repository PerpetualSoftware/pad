/**
 * BUG-3157, the mobile arm: at the mobile breakpoint the picker is a
 * BottomSheet, and BottomSheet does not portal itself. StatusPicker mounts it
 * from a host portaled to <body>; without that host the sheet would render
 * INSIDE the card's <a> (and inside the board's dnd zone), so its rows would be
 * links' descendants. The desktop tests cannot see this: on desktop the Menu
 * portals its own panel.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import StatusPicker from './StatusPicker.svelte';
import { chooseStatus, statusChipIn, statusRows } from './statusPickerTestKit';

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return true;
		},
	},
}));

afterEach(() => cleanup());

function renderInCard() {
	const card = document.createElement('a');
	card.href = '/item';
	card.className = 'item-card';
	document.body.appendChild(card);
	const onselect = vi.fn();
	const screen = render(StatusPicker, {
		target: card,
		props: { value: 'open', options: ['open', 'in_progress', 'done'], onselect },
	});
	return { card, screen, onselect };
}

describe('StatusPicker at the mobile breakpoint', () => {
	it('opens a sheet that is NOT inside the card', async () => {
		const { card } = renderInCard();
		await fireEvent.click(statusChipIn(card));
		await tick();
		const sheet = document.body.querySelector('[role="dialog"]');
		expect(sheet, 'no sheet opened').not.toBeNull();
		expect(card.contains(sheet), 'the sheet rendered inside the card’s <a>').toBe(false);
		expect(statusRows().length).toBe(3);
		expect(statusRows().some((r) => card.contains(r))).toBe(false);
	});

	it('announces a dialog, and its rows are owned by a menu inside it (codex round 1)', async () => {
		const { card } = renderInCard();
		const chip = statusChipIn(card);
		expect(chip.getAttribute('aria-haspopup')).toBe('dialog');
		await fireEvent.click(chip);
		await tick();
		const rows = statusRows();
		expect(rows.length).toBe(3);
		for (const r of rows) {
			const owner = r.closest('[role="menu"]');
			expect(owner, 'a menuitemradio with no owning menu').not.toBeNull();
			expect(owner!.closest('[role="dialog"]'), 'the menu should sit inside the sheet').not.toBeNull();
		}
	});

	it('choosing a row in the sheet writes that status', async () => {
		const { card, onselect } = renderInCard();
		await chooseStatus(card, 'done');
		expect(onselect).toHaveBeenCalledWith('done');
	});
});
