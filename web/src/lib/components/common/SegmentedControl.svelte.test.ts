import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import SegmentedControl from './SegmentedControl.svelte';

const options = [
	{ value: 'day', label: 'Day' },
	{ value: 'week', label: 'Week' },
	{ value: 'fortnight', label: 'Fortnight', disabled: true },
	{ value: 'month', label: 'Month' }
];
const show = (props: Record<string, unknown> = {}) => {
	const onchange = vi.fn();
	const view = render(SegmentedControl, { options, value: 'week', label: 'Range', onchange, ...props });
	return { onchange, ...view };
};
const pressed = () => screen.getAllByRole('button').filter(button => button.getAttribute('aria-pressed') === 'true');

describe('SegmentedControl, pressed mode', () => {
	it('is a labelled group with exactly one pressed button', () => {
		show();
		expect(screen.getByRole('group', { name: 'Range' })).toBeInTheDocument();
		expect(screen.getAllByRole('button')).toHaveLength(4);
		expect(pressed().map(button => button.textContent?.trim())).toEqual(['Week']);
		for (const button of screen.getAllByRole('button')) expect(button).toHaveAttribute('aria-pressed');
	});
	it('follows the value it is given, still with exactly one pressed', async () => {
		const { rerender } = show();
		await rerender({ value: 'month' });
		expect(pressed().map(button => button.textContent?.trim())).toEqual(['Month']);
	});
	it('selects on click and not again for the selected option', async () => {
		const { onchange } = show();
		await fireEvent.click(screen.getByRole('button', { name: 'Day' }));
		expect(onchange).toHaveBeenCalledExactlyOnceWith('day');
		await fireEvent.click(screen.getByRole('button', { name: 'Week' }));
		expect(onchange).toHaveBeenCalledTimes(1);
	});
	it('keeps one tab stop, on the selected option', () => {
		show();
		expect(screen.getAllByRole('button').map(button => button.tabIndex)).toEqual([-1, 0, -1, -1]);
	});
	it('gives the tab stop to the first enabled option when the value matches none', () => {
		show({ value: 'fortnight' });
		expect(screen.getAllByRole('button').map(button => button.tabIndex)).toEqual([0, -1, -1, -1]);
	});
	it('moves and selects with the arrow keys, skipping the disabled option and wrapping', async () => {
		const { onchange, rerender } = show();
		// The control is driven by its value, so follow each change as a parent would.
		const press = async (from: string, key: string, to: string, lands: string) => {
			await fireEvent.keyDown(screen.getByRole('button', { name: from }), { key });
			expect(onchange).toHaveBeenLastCalledWith(to);
			expect(screen.getByRole('button', { name: lands })).toHaveFocus();
			await rerender({ value: to });
			expect(pressed().map(button => button.textContent?.trim())).toEqual([lands]);
			expect(screen.getByRole('button', { name: lands }).tabIndex).toBe(0);
		};
		screen.getByRole('button', { name: 'Week' }).focus();
		await press('Week', 'ArrowRight', 'month', 'Month');
		await press('Month', 'ArrowRight', 'day', 'Day');
		await press('Day', 'ArrowLeft', 'month', 'Month');
		await press('Month', 'ArrowLeft', 'week', 'Week');
		await press('Week', 'ArrowDown', 'month', 'Month');
		await press('Month', 'ArrowUp', 'week', 'Week');
		expect(onchange).toHaveBeenCalledTimes(6);
		expect(onchange.mock.calls.flat()).not.toContain('fortnight');
	});
	it('jumps to the first and last enabled option with Home and End', async () => {
		const { onchange } = show();
		const week = screen.getByRole('button', { name: 'Week' });
		await fireEvent.keyDown(week, { key: 'End' });
		expect(onchange).toHaveBeenLastCalledWith('month');
		expect(screen.getByRole('button', { name: 'Month' })).toHaveFocus();
		await fireEvent.keyDown(screen.getByRole('button', { name: 'Month' }), { key: 'Home' });
		expect(onchange).toHaveBeenLastCalledWith('day');
		expect(screen.getByRole('button', { name: 'Day' })).toHaveFocus();
	});
	it('leaves other keys alone', async () => {
		const { onchange } = show();
		const event = new KeyboardEvent('keydown', { key: 'a', bubbles: true, cancelable: true });
		screen.getByRole('button', { name: 'Week' }).dispatchEvent(event);
		expect(event.defaultPrevented).toBe(false);
		expect(onchange).not.toHaveBeenCalled();
	});
	it('does not select a disabled option', async () => {
		const { onchange } = show();
		const disabled = screen.getByRole('button', { name: 'Fortnight' });
		expect(disabled).toBeDisabled();
		await fireEvent.click(disabled);
		expect(onchange).not.toHaveBeenCalled();
	});
	it('shows a count beside the label', () => {
		show({ options: [{ value: 'a', label: 'Waiting', count: 3 }, { value: 'b', label: 'No date', count: 0 }], value: 'a' });
		expect(screen.getByRole('button', { name: 'Waiting 3' })).toHaveAttribute('aria-pressed', 'true');
		expect(screen.getByRole('button', { name: 'No date 0' })).toBeInTheDocument();
	});
});

describe('SegmentedControl, tablist mode', () => {
	it('exposes a tablist whose tabs carry aria-selected and no aria-pressed', () => {
		show({ mode: 'tablist' });
		expect(screen.getByRole('tablist', { name: 'Range' })).toBeInTheDocument();
		const tabs = screen.getAllByRole('tab');
		expect(tabs.map(tab => tab.getAttribute('aria-selected'))).toEqual(['false', 'true', 'false', 'false']);
		for (const tab of tabs) expect(tab).not.toHaveAttribute('aria-pressed');
		expect(tabs.map(tab => tab.tabIndex)).toEqual([-1, 0, -1, -1]);
		expect(screen.queryByRole('group')).toBeNull();
	});
	it('moves and selects with the arrow keys', async () => {
		const { onchange } = show({ mode: 'tablist' });
		await fireEvent.keyDown(screen.getByRole('tab', { name: 'Week' }), { key: 'ArrowLeft' });
		expect(onchange).toHaveBeenCalledExactlyOnceWith('day');
		expect(screen.getByRole('tab', { name: 'Day' })).toHaveFocus();
	});
	it('links each tab to its panel when given panel ids', () => {
		show({ mode: 'tablist', options: [{ value: 'a', label: 'A', controls: 'panel-a' }, { value: 'b', label: 'B' }], value: 'a' });
		expect(screen.getByRole('tab', { name: 'A' })).toHaveAttribute('aria-controls', 'panel-a');
		expect(screen.getByRole('tab', { name: 'B' })).not.toHaveAttribute('aria-controls');
	});
});
