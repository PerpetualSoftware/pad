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
// through a host component, and the metadata probe is mocked so tests stay
// synchronous AND so the workspace it probes under can be asserted directly —
// at the module boundary rather than at fetch level, since the metadata cache
// is module-global and would dedupe across tests.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Editor } from '@tiptap/core';
import StarterKit from '@tiptap/starter-kit';

const panelOpenMock = vi.fn<(event: Record<string, unknown>) => void>();
const deletionListeners = new Set<(uuid: string) => void>();

vi.mock('$lib/attachments/events', () => ({
	notifyAttachmentSurfaceOpen: (event: Record<string, unknown>) => panelOpenMock(event),
	registerAttachmentDeletionListener: (fn: (uuid: string) => void) => {
		deletionListeners.add(fn);
		return () => deletionListeners.delete(fn);
	},
}));

const probeMock = vi.fn<(ws: string, uuid: string) => Promise<unknown>>();
vi.mock('./attachment-metadata', () => ({
	fetchAttachmentMetadata: (ws: string, uuid: string) => probeMock(ws, uuid),
	invalidateAttachmentMetadata: () => {},
}));

const { AttachmentChip } = await import('./attachment-chip');

const ADDRESS = { workspaceSlug: '', itemId: 'item-A', hostToken: 'apanel-1' };

function makeEditor(element: HTMLElement, address = () => ADDRESS): Editor {
	return new Editor({
		element,
		extensions: [
			StarterKit,
			AttachmentChip.configure({
				// The default address carries no workspace ⇒ no probe, so MIME and
				// size stay null — a LEGITIMATE state the event has to carry
				// (DR-2), not a test shortcut. The workspace test below supplies
				// one.
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

	// The live NodeView chip is a BUTTON, not an anchor: it opens the options
	// panel rather than navigating, and an anchor left the URL reachable by
	// middle-click straight past the panel (orchestrator review of TASK-2424).
	// `renderHTML` — the clipboard / read-only shape — is still an <a download>.
	/**
	 * Repoint the existing chip node at a different attachment via a
	 * transaction, which drives the NodeView's `update()` hook — the path a
	 * collaborative peer's edit takes. Replacing the content instead would
	 * destroy and rebuild the NodeView and prove nothing about `update()`.
	 */
	function repointChip(uuid: string, filename: string) {
		const ed = (editor ??= makeEditor(target));
		let pos = -1;
		ed.state.doc.descendants((node, at) => {
			if (node.type.name === 'attachmentChip') {
				pos = at;
				return false;
			}
			return true;
		});
		if (pos < 0) throw new Error('no chip node in the document');
		const tr = ed.state.tr.setNodeMarkup(pos, undefined, { uuid, filename });
		ed.view.dispatch(tr);
	}

	function chip(): HTMLButtonElement {
		editor ??= makeEditor(target);
		const el = target.querySelector<HTMLButtonElement>('button.file-chip');
		if (!el) throw new Error('chip NodeView did not render');
		return el;
	}

	it('emits the open-surface event instead of opening the file in a new tab', () => {
		const el = chip();
		const opened = vi.spyOn(window, 'open').mockImplementation(() => null);

		el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));

		expect(opened).not.toHaveBeenCalled();
		expect(panelOpenMock).toHaveBeenCalledTimes(1);
		expect(panelOpenMock).toHaveBeenCalledWith({
			attachmentId: 'uuid-1',
			// This editor is configured with no workspace ⇒ no probe.
			workspaceSlug: '',
			// Stamped from the address READER at emit time (DR-8).
			itemId: 'item-A',
			hostToken: 'apanel-1',
			// A single-attachment open on the unified surface (T4a).
			images: [
				{
					id: 'uuid-1',
					alt: 'spec.pdf',
					filename: 'spec.pdf',
					// Null is legitimate: the chip's HEAD probe may not have
					// resolved, and the surface completes what the chip doesn't
					// know (DR-2).
					mime_type: null,
					size_bytes: null,
					width: null,
					height: null,
				},
			],
			index: 0,
			invoker: el,
			filename: 'spec.pdf',
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

	it('probes under the CURRENT workspace after a pane workspace switch', async () => {
		// The pane switches workspace without remounting its editors, and the
		// workspace keys the metadata cache — so a value baked in at configure
		// time makes a mounted chip ask the PREVIOUS workspace about this
		// workspace's attachment, and cache the answer under the wrong key
		// (final review round 3).
		probeMock.mockResolvedValue({ status: 'transient' });
		let ws = 'ws-a';
		editor = makeEditor(target, () => ({ ...ADDRESS, workspaceSlug: ws }));
		chip();
		await Promise.resolve();
		expect(probeMock.mock.calls.map((c) => c[0])).toContain('ws-a');

		probeMock.mockClear();
		ws = 'ws-b';
		repointChip('uuid-3', 'moved.pdf');
		await Promise.resolve();

		const probedWorkspaces = probeMock.mock.calls.map((c) => c[0]);
		expect(probedWorkspaces).toContain('ws-b');
		expect(probedWorkspaces).not.toContain('ws-a');
	});

	it('comes back to life when the node is repointed at a different attachment', () => {
		// `disabled` is what makes a dead chip inert, so the uuid-swap path has
		// to undo it along with everything else markDeleted() set. Leaving it
		// would produce a chip that ANNOUNCES itself as live and does nothing —
		// worse than the dead one, which at least says so. Reachable through a
		// collaborative peer's edit or a ProseMirror node replacement.
		const el = chip();
		for (const fn of deletionListeners) fn('uuid-1');
		expect(el.disabled).toBe(true);

		repointChip('uuid-2', 'other.pdf');

		const live = chip();
		expect(live.disabled).toBe(false);
		expect(live.classList.contains('attachment-missing')).toBe(false);
		live.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 }));
		expect(panelOpenMock).toHaveBeenCalledTimes(1);
		expect(panelOpenMock.mock.calls[0][0]).toMatchObject({ attachmentId: 'uuid-2' });
	});

	it('offers no URL for a middle-click to bypass the panel with', () => {
		// This is why the chip is a button. As an <a href download>, only the
		// primary click ran the handler: middle-click and aux-click still opened
		// or downloaded the file — exactly the accidental download the panel
		// exists to prevent, reachable straight past it. A button has no URL to
		// activate, so the bypass cannot exist rather than being intercepted.
		const el = chip();
		expect(el.tagName).toBe('BUTTON');
		expect(el.hasAttribute('href')).toBe(false);
		expect(el.hasAttribute('download')).toBe(false);
		// type=button: inside a form, a default submit button would be worse
		// than a link.
		expect(el.getAttribute('type')).toBe('button');

		el.dispatchEvent(new MouseEvent('auxclick', { bubbles: true, cancelable: true, button: 1 }));
		expect(panelOpenMock).not.toHaveBeenCalled();
	});

	it('a deleted chip is inert AND unfocusable, not a dead focus stop', () => {
		const el = chip();
		for (const fn of deletionListeners) fn('uuid-1');

		// `disabled` ⇒ not focusable and no events delivered at all, and no
		// tabindex is ever set to put the stop back. The name says what
		// happened rather than promising options.
		expect(el.disabled).toBe(true);
		expect(el.hasAttribute('tabindex')).toBe(false);
		expect(el.getAttribute('aria-label')).toBe('spec.pdf — this attachment has been deleted');
		expect(el.classList.contains('attachment-missing')).toBe(true);

		const click = new MouseEvent('click', { bubbles: true, cancelable: true, detail: 1 });
		el.dispatchEvent(click);
		el.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true }));
		expect(panelOpenMock).not.toHaveBeenCalled();
	});
});
