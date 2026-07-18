import { describe, it, expect } from 'vitest';
import { planHrefClick, type HrefClickContext } from './editorHrefClick';

// A left-click with no modifiers held — the common case.
const leftClick = { button: 0, ctrlKey: false, metaKey: false, shiftKey: false };

// The current-workspace context: workspace "myws" with collections
// tasks/docs/plans (slug → ref prefix).
const COLLS = new Map([
	['tasks', 'TASK'],
	['docs', 'DOC'],
	['plans', 'PLAN'],
]);
function ctx(overrides: Partial<HrefClickContext> = {}): HrefClickContext {
	return { hasOnOpenTarget: true, wsSlug: 'myws', collectionPrefixes: COLLS, ...overrides };
}

describe('planHrefClick — PLAN-2154 Architecture B.3 / TASK-2160', () => {
	it('passthrough when href is empty', () => {
		expect(planHrefClick(leftClick, '', ctx())).toEqual({ kind: 'passthrough' });
	});

	describe('modifier / middle-click behavior — unchanged regardless of context', () => {
		it('passthrough on ctrl-click (new tab)', () => {
			const e = { ...leftClick, ctrlKey: true };
			expect(planHrefClick(e, '/alice/myws/tasks/TASK-5', ctx())).toEqual({ kind: 'passthrough' });
			expect(planHrefClick(e, '/alice/myws/tasks/TASK-5', ctx({ hasOnOpenTarget: false }))).toEqual({
				kind: 'passthrough',
			});
		});

		it('passthrough on cmd/meta-click (new tab)', () => {
			const e = { ...leftClick, metaKey: true };
			expect(planHrefClick(e, '/alice/myws/tasks/TASK-5', ctx())).toEqual({ kind: 'passthrough' });
		});

		it('passthrough on shift-click (new window)', () => {
			const e = { ...leftClick, shiftKey: true };
			expect(planHrefClick(e, '/alice/myws/tasks/TASK-5', ctx())).toEqual({ kind: 'passthrough' });
		});

		it('passthrough on middle-click (button 1)', () => {
			const e = { ...leftClick, button: 1 };
			expect(planHrefClick(e, '/alice/myws/tasks/TASK-5', ctx())).toEqual({ kind: 'passthrough' });
		});
	});

	describe('same-workspace item link with a pane-navigate handler wired', () => {
		it('drills the pane for a username-prefixed item link', () => {
			expect(planHrefClick(leftClick, '/alice/myws/tasks/TASK-5', ctx())).toEqual({
				kind: 'pane',
				href: '/alice/myws/tasks/TASK-5',
			});
		});

		it('drills the pane for a workspace-root-relative (no username) item link', () => {
			expect(planHrefClick(leftClick, '/myws/tasks/TASK-5', ctx())).toEqual({
				kind: 'pane',
				href: '/myws/tasks/TASK-5',
			});
		});

		it('drills the pane for a link into a DIFFERENT (but same-workspace) collection', () => {
			expect(planHrefClick(leftClick, '/alice/myws/docs/DOC-9', ctx())).toEqual({
				kind: 'pane',
				href: '/alice/myws/docs/DOC-9',
			});
		});
	});

	describe('no pane-navigate handler wired (e.g. unembedded full-page view)', () => {
		it('falls back to goto for an internal item link', () => {
			expect(
				planHrefClick(leftClick, '/alice/myws/tasks/TASK-5', ctx({ hasOnOpenTarget: false })),
			).toEqual({ kind: 'goto', href: '/alice/myws/tasks/TASK-5' });
		});
	});

	describe('cross-workspace links — never drill the current pane (Codex review)', () => {
		it('falls back to goto for a /-/r/{workspace}/{ref} resolver link', () => {
			expect(planHrefClick(leftClick, '/-/r/other-workspace/TASK-9', ctx())).toEqual({
				kind: 'goto',
				href: '/-/r/other-workspace/TASK-9',
			});
		});

		it('falls back to goto for a DIFFERENT workspace item as a plain path (would open the wrong item)', () => {
			// /bob/otherws/tasks/TASK-9 is a real, internal, ref-shaped link, but
			// to a DIFFERENT workspace. Drilling would set ?item=TASK-9 on the
			// CURRENT workspace's collection page — a different item numbered 9.
			expect(planHrefClick(leftClick, '/bob/otherws/tasks/TASK-9', ctx())).toEqual({
				kind: 'goto',
				href: '/bob/otherws/tasks/TASK-9',
			});
		});
	});

	describe('internal non-item routes — not misrouted into a pane target (Codex review)', () => {
		it('a non-item route with a ref-shaped tail (e.g. /tags/TASK-5) falls back to goto', () => {
			// `tags` is not a collection slug, so this is a tags route, not an
			// item — a trailing-REF-shape check alone would wrongly intercept it.
			expect(planHrefClick(leftClick, '/alice/myws/tags/TASK-5', ctx())).toEqual({
				kind: 'goto',
				href: '/alice/myws/tags/TASK-5',
			});
		});

		it('a workspace settings-style link falls back to goto', () => {
			expect(planHrefClick(leftClick, '/alice/myws/settings', ctx())).toEqual({
				kind: 'goto',
				href: '/alice/myws/settings',
			});
		});

		it('a bare top-level app route falls back to goto', () => {
			expect(planHrefClick(leftClick, '/console', ctx())).toEqual({
				kind: 'goto',
				href: '/console',
			});
		});

		it('a slug-only (no ref) item link falls back to goto — a graceful degradation, not a break', () => {
			expect(planHrefClick(leftClick, '/alice/myws/docs/some-slug-title', ctx())).toEqual({
				kind: 'goto',
				href: '/alice/myws/docs/some-slug-title',
			});
		});

		it('a self-inconsistent collection/ref path (e.g. /playbooks/TASK-9) falls back to goto (Codex review)', () => {
			// `TASK-9` is a task, not a playbook — even if `playbooks` were a
			// known collection, the ref prefix must match the collection.
			expect(planHrefClick(leftClick, '/alice/myws/docs/TASK-9', ctx())).toEqual({
				kind: 'goto',
				href: '/alice/myws/docs/TASK-9',
			});
		});

		it('a zero-number ref (e.g. TASK-0) falls back to goto (Codex review)', () => {
			expect(planHrefClick(leftClick, '/alice/myws/tasks/TASK-0', ctx())).toEqual({
				kind: 'goto',
				href: '/alice/myws/tasks/TASK-0',
			});
		});
	});

	describe('missing context — declines to drill, falls back to goto', () => {
		it('empty wsSlug falls back to goto', () => {
			expect(planHrefClick(leftClick, '/alice/myws/tasks/TASK-5', ctx({ wsSlug: '' }))).toEqual({
				kind: 'goto',
				href: '/alice/myws/tasks/TASK-5',
			});
		});

		it('empty collectionPrefixes falls back to goto', () => {
			expect(
				planHrefClick(leftClick, '/alice/myws/tasks/TASK-5', ctx({ collectionPrefixes: new Map() })),
			).toEqual({ kind: 'goto', href: '/alice/myws/tasks/TASK-5' });
		});
	});

	describe('external URLs — unchanged regardless of context', () => {
		it('a protocol-relative URL ("//host/...") is external, not internal', () => {
			expect(planHrefClick(leftClick, '//evil.example.com/x', ctx())).toEqual({
				kind: 'external',
				href: '//evil.example.com/x',
			});
		});

		it('an absolute external URL is external', () => {
			expect(planHrefClick(leftClick, 'https://example.com/page', ctx())).toEqual({
				kind: 'external',
				href: 'https://example.com/page',
			});
			expect(
				planHrefClick(leftClick, 'https://example.com/page', ctx({ hasOnOpenTarget: false })),
			).toEqual({ kind: 'external', href: 'https://example.com/page' });
		});

		it('a mailto: link is external', () => {
			expect(planHrefClick(leftClick, 'mailto:a@b.com', ctx())).toEqual({
				kind: 'external',
				href: 'mailto:a@b.com',
			});
		});
	});
});
