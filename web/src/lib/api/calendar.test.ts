import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from './client';

// TASK-4239: the calendar read sends the route's parameters and the page's
// filters as one query string, and drops empty values.

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('api.calendar.get', () => {
	it('puts the fields, window and filters on the collection calendar route', async () => {
		const fetchMock = vi.fn(async () => ({
			status: 200,
			ok: true,
			headers: { get: () => 'application/json' },
			json: async () => ({ items: [], entries: [] }),
			text: async () => '{"items":[],"entries":[]}'
		}));
		vi.stubGlobal('fetch', fetchMock);
		await api.calendar.get('ws', 'tasks', {
			date_field: 'due_date',
			end_field: '',
			from: '2031-03-01T00:00:00Z',
			to: '2031-03-31T00:00:00Z',
			status: 'open'
		});
		const url = String((fetchMock.mock.calls[0] as unknown[])[0]);
		expect(url).toContain('/workspaces/ws/collections/tasks/calendar?');
		expect(url).toContain('date_field=due_date');
		expect(url).toContain('status=open');
		expect(url).not.toContain('end_field');
	});
});
