// The live editor chip opens the shared options panel (PLAN-2392 DR-2 / DR-12,
// TASK-2424) — the same panel a strip file tile opens, so an attachment behaves
// the same wherever you meet it.
//
// Driven through a REAL Tiptap editor rather than mocks: the chip is imperative
// NodeView DOM, and the properties under test are all properties of that DOM —
// which element receives the click, whether it is focusable, and how many times
// one activation fires. A hand-built element would pin none of it.
//
// The bus is mocked so the emission can be asserted as a payload rather than
// through a host component, and the metadata HEAD probe is disabled (no
// workspace slug) so nothing is asynchronous.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

const panelOpenMock = vi.fn<(event: Record<string, unknown>) => void>();
const deletionListeners = new Set<(uuid: string) => void>();

vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentPanelOpen: (event: Record<string, unknown>) => panelOpenMock(event),
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

const { AttachmentChip } = await import('./attachment-chip');

const ADDRESS = { itemId: 'item-A', hostToken: 'apanel-1' };

function makeEditor(element: HTMLElement, address = () => ADDRESS): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentChip.configure({
				// No workspace slug ⇒ no HEAD probe, so MIME and size stay null —
				// which is a LEGITIMATE state the event has to carry (DR-2), not a
				// test shortcut.
				workspaceSlug: '',
				getDownloadUrl: (uuid: string) => `/api/v1/workspaces/ws/attachments/${uuid}`,
				address,
			}),
		],
		content: '<p><a href="pad-attachment:uuid-1">spec.pdf</a></p>',
		editable: true,
	});
}

describe('editor chip → options panel', () => {
	let target: HTMLElement;
	let editor: Editor | undefined;

	beforeEach(() => {
		panelOpenMock.mockReset();
		deletionListeners.clear();
		target = document.body.appendChild(document.createElement('div'));
	});

	afterEach(() => {
		editor?.destroy();
		editor = undefined;
		target.remove();
	});

	function chip(): HTMLAnchorElement {
		editor ??= makeEditor(target);
		const el = target.querySelector<HTMLAnchorElement>('a.file-chip');
		if (!el) throw new Error('chip NodeView did not render');
		return el;
	}

	it('emits the open-panel event instead of opening the file in a new tab', () => {
		const el = chip();
		const opened = vi.spyOn(window, 'open').mockImplementation(() => null);

		el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));

		expect(opened).not.toHaveBeenCalled();
		expect(panelOpenMock).toHaveBeenCalledTimes(1);
		expect(panelOpenMock).toHaveBeenCalledWith({
			attachmentId: 'uuid-1',
			// Stamped from the address READER at emit time (DR-8).
			itemId: 'item-A',
			hostToken: 'apanel-1',
			anchor: el,
			filename: 'spec.pdf',
			// Null is legitimate: the chip's HEAD probe may not have resolved,
			// and the panel completes what the chip doesn't know (DR-2).
			mime_type: null,
			size_bytes: null,
		});
		opened.mockRestore();
	});

	it('suppresses the click so the editor is not navigated', () => {
		const el = chip();
		const event = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
		el.dispatchEvent(event);
		// Editor.svelte's global anchor-click suppressor would eat a plain
		// anchor click; the chip stops propagation so its own activation wins.
		expect(event.defaultPrevented).toBe(true);
	});

	it('reads the address at EMIT time, so a reused composer re-addresses', () => {
		// The comment composer survives an A→B item switch — its `itemId` prop
		// just changes — and Tiptap options cannot be rewritten after
		// configure(). A cached address would send B's chips to A's host.
		const live = { itemId: 'item-A', hostToken: 'apanel-1' };
		editor = makeEditor(target, () => live);
		live.itemId = 'item-B';

		chip().dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));

		expect(panelOpenMock.mock.calls[0][0]).toMatchObject({ itemId: 'item-B' });
	});

	it('activates exactly once from Enter and exactly once from Space', () => {
		const el = chip();

		// Both keys are handled on keydown and both CANCEL the event. That is
		// what holds the count at one: a cancelled keydown produces no
		// activation click, so the keydown and click handlers never both run
		// for one press (the double-fire DR-12 names). Cancelling is also what
		// keeps Enter away from the editor's split-block keymap and Space away
		// from scrolling the page.
		for (const key of ['Enter', ' ']) {
			panelOpenMock.mockReset();
			const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
			el.dispatchEvent(event);
			expect(panelOpenMock).toHaveBeenCalledTimes(1);
			expect(event.defaultPrevented).toBe(true);
		}
	});

	it('leaves modified Space alone — a shortcut is not an activation', () => {
		const el = chip();
		el.dispatchEvent(
			new KeyboardEvent('keydown', { key: ' ', ctrlKey: true, bubbles: true, cancelable: true })
		);
		expect(panelOpenMock).not.toHaveBeenCalled();
	});

	it('names the action, not just the file (DR-12)', () => {
		// Type comes from the filename here, since no HEAD probe ran; size is
		// omitted rather than reported as a confident "0 B".
		expect(chip().getAttribute('aria-label')).toBe('Options for spec.pdf, PDF');
	});

	it('a deleted chip is inert AND unfocusable, not a dead focus stop', () => {
		const el = chip();
		for (const fn of deletionListeners) fn('uuid-1');

		// No href ⇒ not tabbable, and no tabindex is ever set to put the stop
		// back. The name says what happened rather than promising options.
		expect(el.hasAttribute('href')).toBe(false);
		expect(el.hasAttribute('tabindex')).toBe(false);
		expect(el.getAttribute('aria-label')).toBe('spec.pdf — this attachment has been deleted');
		expect(el.classList.contains('attachment-missing')).toBe(true);

		const click = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
		el.dispatchEvent(click);
		el.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true }));
		expect(panelOpenMock).not.toHaveBeenCalled();
		// Still swallowed: the anchor must not navigate to a 404 either.
		expect(click.defaultPrevented).toBe(true);
	});
});
