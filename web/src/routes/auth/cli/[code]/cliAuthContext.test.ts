import { describe, it, expect } from 'vitest';
import { requestedAgo, requestedLine } from './cliAuthContext';

describe('CLI sign-in request context (TASK-2253)', () => {
	const now = new Date('2026-10-09T12:00:00Z');

	it('says how long ago the request was made', () => {
		expect(requestedAgo('2026-10-09T11:59:40Z', now)).toBe('just now');
		expect(requestedAgo('2026-10-09T11:59:00Z', now)).toBe('1 minute ago');
		expect(requestedAgo('2026-10-09T11:46:00Z', now)).toBe('14 minutes ago');
		expect(requestedAgo('2026-10-09T10:30:00Z', now)).toBe('1 hour ago');
		expect(requestedAgo('not a time', now)).toBe('');
	});

	it('names the address when the server reported one, and says nothing when it reported nothing', () => {
		expect(requestedLine('2026-10-09T11:58:00Z', '203.0.113.4', now)).toBe('Requested 2 minutes ago from 203.0.113.4.');
		expect(requestedLine('2026-10-09T11:58:00Z', '', now)).toBe('Requested 2 minutes ago.');
		expect(requestedLine(undefined, '203.0.113.4', now)).toBe('Requested from 203.0.113.4.');
		expect(requestedLine(undefined, undefined, now)).toBe('');
	});
});
