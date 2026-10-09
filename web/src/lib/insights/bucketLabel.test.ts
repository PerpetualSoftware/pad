import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import { bucketLabel, bucketLabels } from './bucketLabel';

// TASK-2220 (audit C49). Labels depend on the viewer's zone, so the zone is
// pinned for the hour cases (vitest runs each file in its own worker).
const zone = process.env.TZ;
beforeAll(() => {
	process.env.TZ = 'America/Los_Angeles';
});
afterAll(() => {
	process.env.TZ = zone;
});

describe('bucketLabel', () => {
	it('labels an hour bucket by its LOCAL start, not the UTC key', () => {
		// 16:00 UTC on 19 Jul is 09:00 in Los Angeles (PDT, UTC-7).
		expect(bucketLabel('2026-07-19T16')).toBe('7/19 9h');
		// 03:00 UTC on 20 Jul is still the evening of 19 Jul there: the work
		// is not attributed to tomorrow.
		expect(bucketLabel('2026-07-20T03')).toBe('7/19 20h');
	});

	it('labels a day bucket by its date', () => {
		expect(bucketLabel('2026-06-20')).toBe('6/20');
		expect(bucketLabel('2026-12-01')).toBe('12/1');
	});

	it('shows any other key as it came', () => {
		expect(bucketLabel('2026-W25')).toBe('2026-W25');
		expect(bucketLabel('')).toBe('');
	});
});

describe('bucketLabels', () => {
	it('keeps every bucket its own band when a local hour repeats (DST fall-back)', () => {
		// 1 Nov 2026: 01:00 PDT (08:00 UTC) and 01:00 PST (09:00 UTC).
		expect(bucketLabels(['2026-11-01T08', '2026-11-01T09', '2026-11-01T10'])).toEqual([
			'11/1 1h',
			'11/1 1h (2)',
			'11/1 2h'
		]);
	});

	it('leaves distinct labels alone', () => {
		expect(bucketLabels(['2026-06-20', '2026-06-21'])).toEqual(['6/20', '6/21']);
	});
});

describe('keys the server already bucketed locally (TASK-3524)', () => {
	it('reformats a local hour key without converting it again', () => {
		expect(bucketLabel('2026-07-19T16', true)).toBe('7/19 16h');
		expect(bucketLabels(['2026-07-19T16', '2026-07-19T17'], true)).toEqual(['7/19 16h', '7/19 17h']);
	});

	it('labels a day the same either way', () => {
		expect(bucketLabel('2026-06-20', true)).toBe('6/20');
	});
});
