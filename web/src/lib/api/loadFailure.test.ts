import { describe, expect, it } from 'vitest';
import { loadFailure } from './loadFailure';
import { PadApiError } from './client';

describe('loadFailure (TASK-2203)', () => {
	it('a refusal names the permission and offers no retry', () => {
		const f = loadFailure('the members list', new PadApiError({ code: 'forbidden', message: 'Forbidden' }));
		expect(f).toEqual({ title: "You don't have access to the members list", detail: 'Ask an owner of this workspace if you need it.', retryable: false });
	});
	it('any other failure carries the server message and a retry', () => {
		const f = loadFailure('tags', new PadApiError({ code: 'rate_limited', message: 'Too many requests' }));
		expect(f).toEqual({ title: "Couldn't load tags", detail: 'Too many requests', retryable: true });
	});
	it('a failure with no message still says what to expect', () => {
		expect(loadFailure('tags', 'boom').detail).toBe('It may be a temporary network or server issue.');
	});
});
