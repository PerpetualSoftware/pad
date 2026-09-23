/**
 * Test helpers for the status chip as a PICKER (BUG-3157). The chip used to
 * cycle to the next status on a click; it now opens a menu, and nothing is
 * written until a row is chosen. Tests that drove the chip go through here so
 * the contract lives in one place.
 */
import { fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { expect } from 'vitest';
import { formatFieldLabel } from '$lib/utils/fieldColors';

/** The chip rendered as a status-picker trigger (and only then). */
export const STATUS_CHIP = '[aria-haspopup="menu"][title="Change status"]';

export function statusChipIn(container: ParentNode): HTMLElement {
	const chip = container.querySelector(STATUS_CHIP);
	expect(chip, 'no status picker chip rendered — this test cannot discriminate without one').not.toBeNull();
	return chip as HTMLElement;
}

/** The open picker's rows. The menu is portaled, so they live under <body>. */
export function statusRows(): HTMLElement[] {
	return Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitemradio"]'));
}

export function rowLabel(row: HTMLElement): string {
	return row.querySelector('.mi-label')?.textContent?.trim() ?? '';
}

/** Click the chip and return the picker's rows. */
export async function openStatusPicker(container: ParentNode): Promise<HTMLElement[]> {
	await fireEvent.click(statusChipIn(container));
	await tick();
	const rows = statusRows();
	expect(rows.length, 'the chip click did not open a status picker').toBeGreaterThan(0);
	return rows;
}

/** Open the picker and choose `status` (by its displayed label). */
export async function chooseStatus(container: ParentNode, status: string): Promise<void> {
	const rows = await openStatusPicker(container);
	const label = formatFieldLabel(status);
	const row = rows.find((r) => rowLabel(r) === label);
	expect(row, `no "${label}" row in the picker; rows: ${rows.map(rowLabel).join(', ')}`).toBeTruthy();
	await fireEvent.click(row!);
	await tick();
}
