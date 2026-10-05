import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/svelte';
import TimelineCommentCard from './TimelineCommentCard.svelte';
import { whoOf, sameWriter } from './historyEvents';
import { viaAppTitle } from '$lib/utils/viaApp';
import type { Activity, Comment, TimelineEntry, Version } from '$lib/types';

/**
 * SPEC-6 U9c (TASK-3413). A write made through an installed app keeps its
 * author; the app is a quiet "via <App>" label beside it (Dave §11 Q4), and
 * history never merges a person's own edit with their edit through an app.
 */
function comment(overrides: Partial<Comment> = {}): Comment {
	return {
		id: 'c-1',
		item_id: 'item-1',
		workspace_id: 'ws-1',
		author: 'Dana',
		body: 'following up',
		created_by: 'user',
		source: 'app',
		created_at: '2026-10-05T12:00:00Z',
		updated_at: '2026-10-05T12:00:00Z',
		...overrides
	};
}

const noop = () => {};

function renderCard(c: Comment) {
	return render(TimelineCommentCard, {
		comment: c,
		wsSlug: 'ws',
		items: [],
		onDelete: noop,
		onReply: noop,
		onEdit: noop,
		onReaction: noop,
		onRemoveReaction: noop
	});
}

describe('via <App> on comments', () => {
	it('labels a comment and a reply written through an app, keeping the person as author', () => {
		const reply = comment({ id: 'c-2', parent_id: 'c-1', author: 'Lee', via_app: 'inst-1', via_app_name: 'Support Portal' });
		const { getAllByTitle, getByText } = renderCard({
			...comment({ via_app: 'inst-1', via_app_name: 'Support Portal' }),
			replies: [reply]
		} as Comment);
		const labels = getAllByTitle(viaAppTitle('Support Portal'));
		expect(labels).toHaveLength(2);
		expect(labels[0].textContent).toBe('via Support Portal');
		// The author stays the person: one author line, not a second one.
		expect(getByText('Dana')).toBeTruthy();
	});

	it('does not label a comment written without an app', () => {
		const { queryByText } = renderCard(comment({ source: 'web' }));
		expect(queryByText(/^via /)).toBeNull();
	});
});

describe('via <App> in history', () => {
	function versionEntry(id: string, via?: { id: string; name: string }): TimelineEntry {
		const v: Version = {
			id,
			document_id: 'item-1',
			content: '',
			change_summary: '',
			created_by: 'user',
			source: 'web',
			is_diff: false,
			created_at: '2026-10-05T12:00:00Z',
			actor_name: 'Dana',
			via_app: via?.id,
			via_app_name: via?.name
		};
		return { kind: 'version', version: v } as TimelineEntry;
	}

	const portal = { id: 'inst-1', name: 'Support Portal' };

	it('carries the app and its name on a version', () => {
		const w = whoOf(versionEntry('v1', portal));
		expect(w.viaApp).toBe('inst-1');
		expect(w.viaAppName).toBe('Support Portal');
		expect(whoOf(versionEntry('v2')).viaApp).toBe('');
	});

	it("never merges a person's own edit with their edit through an app, or two apps", () => {
		const direct = whoOf(versionEntry('v1'));
		const viaPortal = whoOf(versionEntry('v2', portal));
		const viaOther = whoOf(versionEntry('v3', { id: 'inst-2', name: 'Other' }));
		expect(sameWriter(direct, viaPortal)).toBe(false);
		expect(sameWriter(viaPortal, viaOther)).toBe(false);
		expect(sameWriter(viaPortal, whoOf(versionEntry('v4', portal)))).toBe(true);
		expect(sameWriter(direct, whoOf(versionEntry('v5')))).toBe(true);
	});

	it('an activity, which cannot say, still joins either', () => {
		const activity = {
			kind: 'activity',
			activity: { id: 'a1', action: 'updated', actor: 'user', actor_name: 'Dana', source: 'web', metadata: '{}', created_at: '2026-10-05T12:00:00Z' } as unknown as Activity
		} as TimelineEntry;
		const a = whoOf(activity);
		expect(a.viaApp).toBeUndefined();
		expect(sameWriter(a, whoOf(versionEntry('v1', portal)))).toBe(true);
		expect(sameWriter(a, whoOf(versionEntry('v2')))).toBe(true);
	});
});
