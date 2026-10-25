// TASK-4044: the unread pill.
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import UnreadBadge from './UnreadBadge.svelte';

afterEach(cleanup);

describe('UnreadBadge (TASK-4044)', () => {
	it('renders nothing at 0', () => {
		const { container } = render(UnreadBadge, { props: { count: 0 } });
		expect(container.querySelector('.unread-badge')).toBeNull();
	});

	it('shows the count with an accessible label', () => {
		const { getByLabelText } = render(UnreadBadge, { props: { count: 3 } });
		expect(getByLabelText('3 unread').textContent).toBe('3');
	});

	it('caps at 99+ but keeps the real count in the label', () => {
		const { getByLabelText } = render(UnreadBadge, { props: { count: 100 } });
		expect(getByLabelText('100 unread').textContent).toBe('99+');
	});

	it('shows 99 as 99', () => {
		const { getByLabelText } = render(UnreadBadge, { props: { count: 99 } });
		expect(getByLabelText('99 unread').textContent).toBe('99');
	});
});
