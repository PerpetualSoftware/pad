// TASK-4315 PR 3b: the page's address while Chat is open over it.
import { describe, it, expect, beforeEach } from 'vitest';
import { page } from '$app/state';
import { currentHref, currentSearch, currentPathSearch } from './pageLocation';

beforeEach(() => {
	(page as { shallow: unknown }).shallow = null;
	page.url = new URL('http://localhost/alice/ws/tasks?status=done');
	window.history.replaceState(null, '', '/alice/ws/tasks?live=1#h');
});

describe('pageLocation', () => {
	it('with no shallow navigation it reads window.location (raw history writes included)', () => {
		expect(currentHref()).toBe(window.location.href);
		expect(currentSearch()).toBe('?live=1');
		expect(currentPathSearch()).toBe('/alice/ws/tasks?live=1');
	});

	it('under a shallow Chat it reads page.url, not the Chat address in the bar', () => {
		window.history.replaceState(null, '', '/messages/7');
		(page as { shallow: unknown }).shallow = {
			url: new URL('http://localhost/messages/7'),
			route: null,
			params: null
		};
		expect(currentHref()).toBe('http://localhost/alice/ws/tasks?status=done');
		expect(currentSearch()).toBe('?status=done');
		expect(currentPathSearch()).toBe('/alice/ws/tasks?status=done');
	});
});
