import { describe, expect, it } from 'vitest';
import {
	createAttachmentHostToken,
	isAttachmentPanelEventForHost,
	isAttachmentViewerEventForHost,
	notifyAttachmentPanelOpen,
	notifyViewerOpen,
	registerAttachmentPanelListener,
	registerAttachmentViewerListener,
	type AttachmentPanelOpenEvent,
	type AttachmentViewerOpenEvent,
	type LightboxImage,
} from './events';

/**
 * The addressing layer for the attachment options panel (PLAN-2392 DR-8 /
 * TASK-2421).
 *
 * The thing under test is NOT "does a bus deliver" — it's that two
 * simultaneously-mounted ItemDetail hosts (a master and a peeked pane, which
 * can be showing the SAME item) each consume only their own surfaces' events.
 * Every failure mode below has a concrete two-panel bug behind it.
 */

function event(over: Partial<AttachmentPanelOpenEvent> = {}): AttachmentPanelOpenEvent {
	return {
		attachmentId: 'att-1',
		itemId: 'item-1',
		hostToken: 'host-a',
		anchor: null,
		filename: 'notes.pdf',
		mime_type: 'application/pdf',
		size_bytes: 1234,
		...over,
	};
}

describe('createAttachmentHostToken', () => {
	it('mints a distinct, non-empty token per call', () => {
		const seen = new Set<string>();
		for (let i = 0; i < 100; i++) {
			const token = createAttachmentHostToken();
			expect(token).toBeTruthy();
			expect(seen.has(token)).toBe(false);
			seen.add(token);
		}
	});
});

describe('isAttachmentPanelEventForHost', () => {
	const host = { itemId: 'item-1', hostToken: 'host-a' };

	it('matches when BOTH the item and the token are the host’s', () => {
		expect(isAttachmentPanelEventForHost(event(), host)).toBe(true);
	});

	it('ignores an event that matches only the item (the two-panes-one-item case)', () => {
		// Master and peeked pane showing the same item: itemId alone is not an
		// address, or one tap opens two panels.
		expect(isAttachmentPanelEventForHost(event({ hostToken: 'host-b' }), host)).toBe(false);
	});

	it('ignores an event that matches only the token', () => {
		// One host, but the emitting surface belongs to a different item —
		// e.g. a stale NodeView configured before an item switch.
		expect(isAttachmentPanelEventForHost(event({ itemId: 'item-2' }), host)).toBe(false);
	});

	it('never matches when the EVENT carries no token', () => {
		// An unconfigured NodeView (options default to '') must not be able to
		// address every host at once.
		expect(isAttachmentPanelEventForHost(event({ hostToken: '' }), host)).toBe(false);
	});

	it('never matches when the HOST has no token', () => {
		expect(isAttachmentPanelEventForHost(event(), { itemId: 'item-1', hostToken: '' })).toBe(false);
		expect(isAttachmentPanelEventForHost(event(), { itemId: 'item-1', hostToken: null })).toBe(
			false
		);
		expect(
			isAttachmentPanelEventForHost(event(), { itemId: 'item-1', hostToken: undefined })
		).toBe(false);
	});

	it('never matches when either side has no item', () => {
		expect(isAttachmentPanelEventForHost(event({ itemId: '' }), host)).toBe(false);
		expect(isAttachmentPanelEventForHost(event(), { itemId: null, hostToken: 'host-a' })).toBe(
			false
		);
	});
});

describe('the panel channel with two live hosts', () => {
	it('delivers one surface’s event to exactly one of two hosts on the same item', () => {
		const master = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const peeked = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const masterSeen: AttachmentPanelOpenEvent[] = [];
		const peekedSeen: AttachmentPanelOpenEvent[] = [];

		const offMaster = registerAttachmentPanelListener((e) => {
			if (isAttachmentPanelEventForHost(e, master)) masterSeen.push(e);
		});
		const offPeeked = registerAttachmentPanelListener((e) => {
			if (isAttachmentPanelEventForHost(e, peeked)) peekedSeen.push(e);
		});

		try {
			notifyAttachmentPanelOpen(event({ hostToken: peeked.hostToken }));
			expect(masterSeen).toHaveLength(0);
			expect(peekedSeen).toHaveLength(1);
			expect(peekedSeen[0].attachmentId).toBe('att-1');

			notifyAttachmentPanelOpen(
				event({ attachmentId: 'att-2', hostToken: master.hostToken })
			);
			expect(masterSeen).toHaveLength(1);
			expect(masterSeen[0].attachmentId).toBe('att-2');
			expect(peekedSeen).toHaveLength(1);
		} finally {
			offMaster();
			offPeeked();
		}
	});

	it('carries nullable metadata through unchanged (a chip’s HEAD probe may be incomplete)', () => {
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		let received: AttachmentPanelOpenEvent | null = null;
		const off = registerAttachmentPanelListener((e) => {
			if (isAttachmentPanelEventForHost(e, host)) received = e;
		});
		try {
			notifyAttachmentPanelOpen(
				event({
					hostToken: host.hostToken,
					filename: null,
					mime_type: null,
					size_bytes: null,
				})
			);
		} finally {
			off();
		}
		expect(received).not.toBeNull();
		expect(received!.filename).toBeNull();
		expect(received!.mime_type).toBeNull();
		expect(received!.size_bytes).toBeNull();
	});

	it('drops an unaddressable emission rather than broadcasting it', () => {
		const seen: AttachmentPanelOpenEvent[] = [];
		const off = registerAttachmentPanelListener((e) => seen.push(e));
		try {
			notifyAttachmentPanelOpen(event({ hostToken: '' }));
			notifyAttachmentPanelOpen(event({ itemId: '' }));
			notifyAttachmentPanelOpen(event({ attachmentId: '' }));
		} finally {
			off();
		}
		expect(seen).toHaveLength(0);
	});

	it('stops delivering after dispose', () => {
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const seen: AttachmentPanelOpenEvent[] = [];
		const off = registerAttachmentPanelListener((e) => {
			if (isAttachmentPanelEventForHost(e, host)) seen.push(e);
		});
		notifyAttachmentPanelOpen(event({ hostToken: host.hostToken }));
		off();
		notifyAttachmentPanelOpen(event({ hostToken: host.hostToken }));
		expect(seen).toHaveLength(1);
	});
});

/**
 * The addressing layer for the image viewer (PLAN-2392 phase 3a / TASK-2428).
 *
 * Same shape of test as the panel channel above, and for the same reason: the
 * bus is module-global while `ItemDetail` is mounted twice (master + peeked
 * pane), so every case below has a concrete two-viewer bug behind it. The two
 * channels are exercised separately because they are separate predicates —
 * a shared rule stated twice is exactly what regresses in one place only.
 */
function image(over: Partial<LightboxImage> = {}): LightboxImage {
	return {
		id: 'att-1',
		alt: 'a diagram',
		filename: 'diagram.png',
		mime_type: 'image/png',
		size_bytes: 4096,
		width: 800,
		height: 600,
		...over,
	};
}

function viewerEvent(over: Partial<AttachmentViewerOpenEvent> = {}): AttachmentViewerOpenEvent {
	return {
		attachmentId: 'att-1',
		workspaceSlug: 'ws-1',
		itemId: 'item-1',
		hostToken: 'host-a',
		images: [image()],
		index: 0,
		invoker: null,
		...over,
	};
}

describe('isAttachmentViewerEventForHost', () => {
	const host = { itemId: 'item-1', hostToken: 'host-a' };

	it('matches when BOTH the item and the token are the host’s', () => {
		expect(isAttachmentViewerEventForHost(viewerEvent(), host)).toBe(true);
	});

	it('ignores an event that matches only the item (the two-panes-one-item case)', () => {
		expect(isAttachmentViewerEventForHost(viewerEvent({ hostToken: 'host-b' }), host)).toBe(
			false
		);
	});

	it('ignores an event that matches only the token', () => {
		expect(isAttachmentViewerEventForHost(viewerEvent({ itemId: 'item-2' }), host)).toBe(false);
	});

	it('never matches on a missing half, on either side', () => {
		// Two absences are not a match: a surface given no token must not be
		// able to address every host at once, and a host without one must not
		// consume unaddressed events.
		expect(isAttachmentViewerEventForHost(viewerEvent({ hostToken: '' }), host)).toBe(false);
		expect(isAttachmentViewerEventForHost(viewerEvent({ itemId: '' }), host)).toBe(false);
		expect(
			isAttachmentViewerEventForHost(viewerEvent(), { itemId: 'item-1', hostToken: '' })
		).toBe(false);
		expect(isAttachmentViewerEventForHost(viewerEvent(), { itemId: null, hostToken: null })).toBe(
			false
		);
		expect(
			isAttachmentViewerEventForHost(viewerEvent({ hostToken: '', itemId: '' }), {
				itemId: '',
				hostToken: '',
			})
		).toBe(false);
	});

	it('tolerates a null/undefined event', () => {
		expect(isAttachmentViewerEventForHost(null, host)).toBe(false);
		expect(isAttachmentViewerEventForHost(undefined, host)).toBe(false);
	});
});

describe('viewer open channel', () => {
	it('delivers to the addressed host only, with two hosts subscribed', () => {
		const master = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const peeked = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const masterSeen: AttachmentViewerOpenEvent[] = [];
		const peekedSeen: AttachmentViewerOpenEvent[] = [];
		const offMaster = registerAttachmentViewerListener((e) => {
			if (isAttachmentViewerEventForHost(e, master)) masterSeen.push(e);
		});
		const offPeeked = registerAttachmentViewerListener((e) => {
			if (isAttachmentViewerEventForHost(e, peeked)) peekedSeen.push(e);
		});
		try {
			notifyViewerOpen(viewerEvent({ hostToken: peeked.hostToken }));
			expect(masterSeen).toHaveLength(0);
			expect(peekedSeen).toHaveLength(1);

			notifyViewerOpen(viewerEvent({ attachmentId: 'att-2', hostToken: master.hostToken }));
			expect(masterSeen).toHaveLength(1);
			expect(masterSeen[0].attachmentId).toBe('att-2');
			expect(peekedSeen).toHaveLength(1);
		} finally {
			offMaster();
			offPeeked();
		}
	});

	it('carries the emit-time workspace, the set, the index and the invoker through unchanged', () => {
		// The workspace is CAPTURED, not resolved by the host: the pane can
		// switch workspace without remounting, so a host that read it live
		// could serve a viewer opened in ws1 from ws2's endpoint.
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		// A stand-in element: this suite runs in the node environment, and the
		// bus only ever carries the reference — the host is where it is used.
		const invoker = { tagName: 'IMG' } as unknown as HTMLElement;
		const images = [image(), image({ id: 'att-2', alt: 'second' })];
		let received: AttachmentViewerOpenEvent | null = null;
		const off = registerAttachmentViewerListener((e) => {
			if (isAttachmentViewerEventForHost(e, host)) received = e;
		});
		try {
			notifyViewerOpen(
				viewerEvent({
					attachmentId: 'att-2',
					workspaceSlug: 'other-ws',
					hostToken: host.hostToken,
					images,
					index: 1,
					invoker,
				})
			);
		} finally {
			off();
		}
		const got = received as AttachmentViewerOpenEvent | null;
		expect(got).not.toBeNull();
		expect(got!.workspaceSlug).toBe('other-ws');
		expect(got!.images).toEqual(images);
		expect(got!.index).toBe(1);
		// The event's own invariant: the index names the attachment it opened on.
		expect(got!.images[got!.index]?.id).toBe(got!.attachmentId);
		expect(got!.invoker).toBe(invoker);
	});

	it('passes incomplete image metadata through as nulls', () => {
		// An inline image knows only what its NodeView options give it, and its
		// HEAD probe may not have completed. The viewer opens anyway.
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		let received: AttachmentViewerOpenEvent | null = null;
		const off = registerAttachmentViewerListener((e) => {
			if (isAttachmentViewerEventForHost(e, host)) received = e;
		});
		try {
			notifyViewerOpen(
				viewerEvent({
					hostToken: host.hostToken,
					images: [
						image({
							filename: null,
							mime_type: null,
							size_bytes: null,
							width: null,
							height: null,
						}),
					],
				})
			);
		} finally {
			off();
		}
		const got = received as AttachmentViewerOpenEvent | null;
		expect(got).not.toBeNull();
		expect(got!.images[0].filename).toBeNull();
		expect(got!.images[0].width).toBeNull();
	});

	it('drops an unaddressable or empty emission rather than broadcasting it', () => {
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => seen.push(e));
		try {
			notifyViewerOpen(viewerEvent({ hostToken: '' }));
			notifyViewerOpen(viewerEvent({ itemId: '' }));
			notifyViewerOpen(viewerEvent({ attachmentId: '' }));
			// A full-screen viewer with nothing to show is worse than no viewer.
			notifyViewerOpen(viewerEvent({ images: [] }));
			// The slug is a path segment of every image URL, and the host does
			// not substitute its own: without it the viewer opens on 404s.
			notifyViewerOpen(viewerEvent({ workspaceSlug: '' }));
		} finally {
			off();
		}
		expect(seen).toHaveLength(0);
	});

	it('returns a disposer that stops delivery', () => {
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => {
			if (isAttachmentViewerEventForHost(e, host)) seen.push(e);
		});
		notifyViewerOpen(viewerEvent({ hostToken: host.hostToken }));
		expect(typeof off).toBe('function');
		off();
		notifyViewerOpen(viewerEvent({ hostToken: host.hostToken }));
		expect(seen).toHaveLength(1);
		// Disposing twice is a no-op, not a throw — teardown paths can double-fire.
		expect(() => off()).not.toThrow();
	});

	it('keeps the two channels separate', () => {
		// One host, both channels: a panel emission must not open a viewer and
		// a viewer emission must not open a panel, even though the identity
		// fields (and the TOKEN — one per host, not one per channel) are shared.
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const viewerSeen: AttachmentViewerOpenEvent[] = [];
		const panelSeen: AttachmentPanelOpenEvent[] = [];
		const offViewer = registerAttachmentViewerListener((e) => viewerSeen.push(e));
		const offPanel = registerAttachmentPanelListener((e) => panelSeen.push(e));
		try {
			notifyAttachmentPanelOpen(event({ hostToken: host.hostToken }));
			notifyViewerOpen(viewerEvent({ hostToken: host.hostToken }));
		} finally {
			offViewer();
			offPanel();
		}
		expect(viewerSeen).toHaveLength(1);
		expect(panelSeen).toHaveLength(1);
		expect(viewerSeen[0].attachmentId).toBe('att-1');
	});
});
