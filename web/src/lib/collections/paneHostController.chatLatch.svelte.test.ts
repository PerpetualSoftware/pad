// TASK-4315 PR 3b: a pane gesture must not issue a second history traversal
// while Chat's own close traversal is still pending. history.go has no
// completion promise, so a second go would overshoot the opener.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: false } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { userId: 'u1' } }));

import { page } from '$app/state';
import { goto } from '$app/navigation';
import { chatSurface, __resetChatSurfaceForTests } from '$lib/stores/chatSurface.svelte';
import { createPaneController } from './paneHostController';

const asShallow = (path: string) =>
	({ url: new URL(`http://localhost${path}`), route: null, params: null }) as never;

function controller() {
	return createPaneController({
		getOpenItemRef: () => 'TASK-1',
		cancelFollow: vi.fn(),
		focusPaneRegion: vi.fn(),
		captureReturnFocus: vi.fn(),
		setBypassNavGuard: vi.fn()
	});
}

let go: ReturnType<typeof vi.spyOn>;

/** Chat open over a pane; Close issues Chat's traversal and leaves its latch pending. */
function pendChatTraversal(paneState: Record<string, unknown>) {
	page.url = new URL('http://localhost/alice/ws/tasks?item=TASK-1');
	page.state = { chatDepth: 1, ...paneState };
	(page as { shallow: unknown }).shallow = asShallow('/messages');
	chatSurface.close();
	expect(go).toHaveBeenCalledTimes(1);
}

beforeEach(() => {
	go = vi.spyOn(history, 'go').mockImplementation(() => {});
	page.state = {};
	(page as { shallow: unknown }).shallow = null;
	__resetChatSurfaceForTests();
});
afterEach(() => {
	__resetChatSurfaceForTests();
	vi.restoreAllMocks();
});

describe('a pane gesture during a pending Chat traversal', () => {
	it('closeItemPane (Escape / Close pane) issues no second go', () => {
		const c = controller();
		pendChatTraversal({ paneDepth: 0, paneOwned: true });
		expect(c.paneNavInFlight()).toBe(true);
		c.closeItemPane();
		expect(go).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});

	it('handlePaneBack issues no second go', () => {
		const c = controller();
		pendChatTraversal({ paneDepth: 1, paneOwned: true });
		c.handlePaneBack();
		expect(go).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});

	// Not the reset branch: with the latch pending the top-level guard in
	// openItemPaneByRef is already true, so the open is held there. The reset branch
	// can only run from the continuation, after the latch is released (covered below).
	it('an open at depth>0 is held at the top guard while the latch is pending', () => {
		const c = controller();
		pendChatTraversal({ paneDepth: 2, paneOwned: true });
		c.openItemPaneByRef('TASK-9');
		expect(go).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});

	it('with no Chat traversal pending the same close does traverse', () => {
		const c = controller();
		page.url = new URL('http://localhost/alice/ws/tasks?item=TASK-1');
		page.state = { paneDepth: 0, paneOwned: true };
		c.closeItemPane();
		expect(go).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});
});

describe('a guard read that settles the Chat latch', () => {
	const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));
	/** The traversal has landed (popstate fired, surface closed, key moved) but the surface-key effect has not run. */
	function landed(paneState: Record<string, unknown>) {
		popstate();
		(page as { shallow: unknown }).shallow = null;
		page.state = paneState;
	}

	it('handlePaneBack: the continuation takes the reset go, and Back adds none', () => {
		const c = controller();
		page.url = new URL('http://localhost/alice/ws/tasks?item=TASK-1');
		page.state = { chatDepth: 1, paneDepth: 2, paneOwned: true };
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		// The open at depth 2 parks behind Chat's close, then resets the stack.
		c.openItemPaneByRef('TASK-9');
		expect(go).toHaveBeenCalledTimes(1); // Chat's close
		landed({ paneDepth: 2, paneOwned: true });
		c.handlePaneBack();
		// Chat's close + the continuation's one reset go; Back must not add a third.
		expect(go).toHaveBeenCalledTimes(2);
		c.clearPaneGo();
	});

	it('a gesture reading the guard when the continuation is a goto: exactly one goto', () => {
		const c = controller();
		page.url = new URL('http://localhost/alice/ws/tasks?item=TASK-1');
		page.state = { chatDepth: 1, paneDepth: 0, paneOwned: true };
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		c.navigatePaneTo('TASK-5');
		expect(go).toHaveBeenCalledTimes(1);
		vi.mocked(goto).mockClear();
		landed({ paneDepth: 0, paneOwned: true });
		c.openItemPaneByRef('TASK-9');
		expect(goto).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});

	it('control: no pending latch, the guard reads idle and the gesture proceeds', () => {
		const c = controller();
		page.url = new URL('http://localhost/alice/ws/tasks?item=TASK-1');
		page.state = { paneDepth: 2, paneOwned: true };
		expect(c.paneNavInFlight()).toBe(false);
		c.handlePaneBack();
		expect(go).toHaveBeenCalledTimes(1);
		c.clearPaneGo();
	});
});
