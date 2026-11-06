import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import PendingBubble from './PendingBubble.svelte';
afterEach(cleanup);
it('shows an unconfirmed send with explicit Retry and Discard; neither runs automatically', async () => {
	const onRetry = vi.fn(); const onDiscard = vi.fn();
	const r = render(PendingBubble, { props: {
		pending: { op_key: 'original', body: 'my attempt', status: 'unconfirmed' }, onRetry, onDiscard
	} });
	expect(r.getByRole('status').textContent).toBe('Send not confirmed');
	expect(onRetry).not.toHaveBeenCalled(); expect(onDiscard).not.toHaveBeenCalled();
	await fireEvent.click(r.getByRole('button', { name: 'Retry' })); expect(onRetry).toHaveBeenCalledWith('original');
	await fireEvent.click(r.getByRole('button', { name: 'Discard' })); expect(onDiscard).toHaveBeenCalledWith('original');
});
