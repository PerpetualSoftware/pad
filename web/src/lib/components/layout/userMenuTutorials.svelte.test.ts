// TASK-3452: the user menu's Resources block carries Tutorials right after
// Docs (docs/brand.md order). Cloud opens the in-app library in the same tab;
// self-hosted links out to getpad.dev/learn in a new one, like Docs.
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import UserMenuResources from './UserMenuResources.svelte';

function labels(container: HTMLElement): string[] {
	return [...container.querySelectorAll('a')].map((a) => a.textContent?.trim() ?? '');
}

describe('UserMenuResources: Tutorials', () => {
	it('on Cloud, opens /console/tutorials in the same tab, right after Docs', () => {
		const { container } = render(UserMenuResources, { cloudMode: true });
		const link = screen.getByRole('menuitem', { name: 'Tutorials' });
		expect(link.getAttribute('href')).toBe('/console/tutorials');
		expect(link.getAttribute('target')).toBeNull();
		const order = labels(container);
		expect(order.indexOf('Tutorials')).toBe(order.indexOf('Docs') + 1);
	});

	it('self-hosted, links to getpad.dev/learn in a new tab, right after Docs', () => {
		const { container } = render(UserMenuResources, { cloudMode: false });
		const link = screen.getByRole('menuitem', { name: 'Tutorials' });
		expect(link.getAttribute('href')).toBe('https://getpad.dev/learn');
		expect(link.getAttribute('target')).toBe('_blank');
		const order = labels(container);
		expect(order.indexOf('Tutorials')).toBe(order.indexOf('Docs') + 1);
	});
});
