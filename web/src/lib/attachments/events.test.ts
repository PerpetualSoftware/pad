import { describe, expect, it } from 'vitest';
import type { AttachmentUploadResult } from '$lib/types';
import {
	createAttachmentHostToken,
	isAttachmentPanelEventForHost,
	isAttachmentViewerEventForHost,
	notifyAttachmentPanelOpen,
	notifyViewerOpen,
	registerAttachmentPanelListener,
	registerAttachmentViewerListener,
	toUploadedAttachment,
	type AttachmentPanelOpenEvent,
	type AttachmentViewerOpenEvent,
	type LightboxImage,
	type ViewerOpenRequest,
	type ViewerReadyImage,
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

// Typed as the EMITTER's shape (`ViewerOpenRequest`), not the event's: the
// producer half of this channel must resolve the MIME before it asks for a
// viewer, and that is now a type, so the fixture has to satisfy it. The one
// case that deliberately violates it casts, and asserts the runtime guard.
/**
 * The same member in the PRODUCER's shape. The cast is confined to this one
 * helper: every field but `mime_type` is identical, and the default IS a
 * resolved allowlisted type, so the assertion is true of everything it returns.
 * A case that deliberately wants an unresolved or refused MIME calls `image()`
 * and casts at ITS call site, where the violation is visible.
 */
function viewable(over: Partial<LightboxImage> = {}): ViewerReadyImage {
	return image({ mime_type: 'image/png', ...over }) as ViewerReadyImage;
}

function viewerEvent(over: Partial<ViewerOpenRequest> = {}): ViewerOpenRequest {
	return {
		attachmentId: 'att-1',
		workspaceSlug: 'ws-1',
		itemId: 'item-1',
		hostToken: 'host-a',
		images: [viewable()],
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
		const images = [viewable(), viewable({ id: 'att-2', alt: 'second' })];
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

	it('passes the CAPTION metadata through as nulls', () => {
		// An inline image knows only what its NodeView options give it: there is
		// no filename on the node's attrs and the HEAD probe carries no
		// dimensions. Those are captions — absent is a fact about the producer,
		// not a reason to refuse — so the viewer opens anyway. `mime_type` is
		// deliberately NOT in this list; it is the gate, and the next test is
		// about it.
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
						viewable({
							filename: null,
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

	it('refuses a set whose MIME is unresolved or not allowlisted', () => {
		// THE GATE, at the boundary rather than in each producer (TASK-2433).
		//
		// This test used to assert the opposite — that a null `mime_type` was
		// passed through — on the reasoning that what is viewable is the
		// emitter's judgement. TASK-2431 ended that: `Lightbox` FAILS CLOSED on
		// an unresolved MIME, so an emission the viewer will filter to nothing
		// is not a permissive channel, it is an image that does not open with
		// nothing thrown and nothing logged. A producer that forgets is now
		// refused here, where every producer passes.
		//
		// The casts are the point, not a workaround: `notifyViewerOpen` takes
		// `ViewerOpenRequest`, so each of these is ALREADY a compile error at an
		// honest call site. What is asserted here is the runtime half — the one
		// that still holds for a JS caller, an `any`, or a value that arrived
		// from outside the type system.
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => seen.push(e));
		try {
			// Unresolved: the probe never answered, or the producer never asked.
			notifyViewerOpen(viewerEvent({ images: [image({ mime_type: null }) as ViewerReadyImage] }));
			// Resolved, and refused: `image/svg+xml` can carry active content
			// (DR-16). `image/*` is not sufficient reason to display something.
			notifyViewerOpen(
				viewerEvent({ images: [image({ mime_type: 'image/svg+xml' }) as ViewerReadyImage] })
			);
			notifyViewerOpen(
				viewerEvent({ images: [image({ mime_type: 'application/pdf' }) as ViewerReadyImage] })
			);
			// ONE bad entry poisons the whole emission rather than being filtered
			// out of it: `index` and `attachmentId` name a position in the set the
			// PRODUCER built, and silently renumbering it would open the viewer on
			// a different image than the one the user activated.
			notifyViewerOpen(
				viewerEvent({
					images: [viewable(), image({ id: 'att-2', mime_type: null }) as ViewerReadyImage],
				})
			);
		} finally {
			off();
		}
		expect(seen).toHaveLength(0);
	});

	it('refuses a malformed set instead of throwing out of a notify call', () => {
		// A boundary's input is only as good as its caller, and a THROW here is
		// worse than a drop: it unwinds out of a notify function into whatever
		// the producer was doing — for the inline image NodeView, into the
		// `.then` of its MIME resolution, as an unhandled rejection.
		//
		// The sparse case is the one a type cannot catch and `.some` silently
		// permits: holes are SKIPPED by `.some`, so a hole-only array passes an
		// every-entry check that is written with it and arrives at a viewer with
		// nothing to show.
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => seen.push(e));
		const bad = (images: unknown) =>
			notifyViewerOpen({ ...viewerEvent(), images } as unknown as ViewerOpenRequest);
		try {
			expect(() => bad({ length: 1 })).not.toThrow();
			expect(() => bad(new Array(1))).not.toThrow();
			expect(() => bad([undefined])).not.toThrow();
			expect(() => bad([{ id: 'att-1', alt: '', mime_type: 42 }])).not.toThrow();
			expect(() => bad('image/png')).not.toThrow();
			expect(() => bad(null)).not.toThrow();
		} finally {
			off();
		}
		expect(seen).toHaveLength(0);
	});

	it('delivers a FROZEN set unchanged — readonly is the contract, not a rejection', () => {
		// The set is `readonly` on the event because the viewer must not reorder
		// or mutate something the emitter still owns, so a producer freezing its
		// own array is the contract being honoured, not a malformed input.
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => seen.push(e));
		const images = Object.freeze([viewable()]);
		try {
			notifyViewerOpen(viewerEvent({ images }));
		} finally {
			off();
		}
		expect(seen).toHaveLength(1);
		expect(seen[0].images).toBe(images);
	});

	it('still delivers a set whose every entry is allowlisted', () => {
		// The control. A gate that refused everything would satisfy the test
		// above and close the channel.
		const seen: AttachmentViewerOpenEvent[] = [];
		const off = registerAttachmentViewerListener((e) => seen.push(e));
		try {
			notifyViewerOpen(
				viewerEvent({
					images: [viewable(), viewable({ id: 'att-2', mime_type: 'image/webp' })],
				})
			);
		} finally {
			off();
		}
		expect(seen).toHaveLength(1);
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

describe('toUploadedAttachment (TASK-2459)', () => {
	it('threads the dimensions the narrowing used to drop', () => {
		const out = toUploadedAttachment({
			id: 'a1',
			filename: 'big.png',
			mime: 'image/png',
			size: 4096,
			width: 4000,
			height: 3000,
		} as AttachmentUploadResult);
		expect(out.width).toBe(4000);
		expect(out.height).toBe(3000);
	});

	it('leaves dimensions null when the response omits them', () => {
		const out = toUploadedAttachment({
			id: 'a1',
			filename: 'f.bin',
			mime: 'application/octet-stream',
			size: 10,
		} as AttachmentUploadResult);
		expect(out.width).toBeNull();
		expect(out.height).toBeNull();
	});
});
