import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import FilterBar from './FilterBar.svelte';
import type { Collection } from '$lib/types';

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Tasks',
		slug: 'tasks',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields: [] }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'TASK',
	};
}

function baseProps(overrides: Record<string, unknown> = {}) {
	return {
		collection: collection(),
		activeFilters: {},
		searchQuery: '',
		onFilterChange: vi.fn(),
		onSearchChange: vi.fn(),
		...overrides,
	};
}

afterEach(() => {
	cleanup();
});

describe('FilterBar unparented chip (TASK-2099)', () => {
	it('does not render when unparentedAvailable is false (default / restricted caller)', () => {
		render(FilterBar, { props: baseProps() });
		expect(document.querySelector('.unparented-chip')).toBeNull();
	});

	it('renders when unparentedAvailable is true (unrestricted caller)', () => {
		render(FilterBar, { props: baseProps({ unparentedAvailable: true }) });
		const chip = document.querySelector('.unparented-chip');
		expect(chip).not.toBeNull();
		expect(chip?.textContent).toContain('Unparented only');
	});

	it('reflects unparentedActive via the active class + aria-pressed', () => {
		render(FilterBar, { props: baseProps({ unparentedAvailable: true, unparentedActive: true }) });
		const chip = document.querySelector('.unparented-chip') as HTMLButtonElement;
		expect(chip.classList.contains('active')).toBe(true);
		expect(chip.getAttribute('aria-pressed')).toBe('true');
	});

	it('fires onUnparentedChange with the toggled value on click', async () => {
		const onUnparentedChange = vi.fn();
		render(FilterBar, {
			props: baseProps({ unparentedAvailable: true, unparentedActive: false, onUnparentedChange }),
		});
		const chip = document.querySelector('.unparented-chip') as HTMLButtonElement;
		await fireEvent.click(chip);
		await tick();
		expect(onUnparentedChange).toHaveBeenCalledWith(true);
	});

	it('a restricted caller (unparentedAvailable=false) never sees the chip, even if active=true is passed', () => {
		// Defensive case: a stale/incorrect caller still can't render it —
		// availability is the sole gate (DR-2), not activeness.
		render(FilterBar, { props: baseProps({ unparentedAvailable: false, unparentedActive: true }) });
		expect(document.querySelector('.unparented-chip')).toBeNull();
	});
});
