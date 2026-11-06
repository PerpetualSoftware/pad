// TASK-4315 PR 3b: a pane gesture must not issue a second history traversal
// while Chat's own close traversal is still pending. history.go has no
// completion promise, so a second go would overshoot the opener.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: false } }));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { userId: 'u1' } }));

import { page } from '$app/state';
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

	it('the depth>0 reset path of an open issues no second go', () => {
		const c = controller();
		pendChatTraversal({ paneDepth: 2, paneOwned: true });
		// The surface has since closed under the pending latch, so the open runs itself.
		(page as { shallow: unknown }).shallow = null;
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
