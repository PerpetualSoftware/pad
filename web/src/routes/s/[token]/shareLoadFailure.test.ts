import { describe, expect, it } from 'vitest';
import { LINK_GONE, shareLoadFailure } from './shareLoadFailure';

const apiError = (code: string, message = 'msg') => Object.assign(new Error(message), { code });

// TASK-2249 (audit C85): what a share page says when its link won't load.
describe('shareLoadFailure', () => {
	it("the server's single 404 reads as a gone link, with what to do and no retry", () => {
		const f = shareLoadFailure(apiError('not_found', 'Not found'));
		expect(f).toBe(LINK_GONE);
		expect(f.message).toContain('Ask the person who shared it');
		expect(f.retryable).toBe(false);
	});

	it('a dropped connection or timeout is not reported as a bad link, and can be retried', () => {
		for (const err of [new TypeError('Failed to fetch'), new Error('API error: 502'), undefined]) {
			const f = shareLoadFailure(err);
			expect(f.title).toBe("Couldn't reach Pad");
			expect(f.message).not.toContain('Failed to fetch');
			expect(f.retryable).toBe(true);
		}
	});

	it('rate limiting says so and can be retried', () => {
		expect(shareLoadFailure(apiError('rate_limited')).retryable).toBe(true);
	});

	it("another structured refusal keeps the server's own sentence", () => {
		const f = shareLoadFailure(apiError('forbidden', 'This share is disabled for your workspace.'));
		expect(f.message).toBe('This share is disabled for your workspace.');
		expect(f.retryable).toBe(false);
	});
});
