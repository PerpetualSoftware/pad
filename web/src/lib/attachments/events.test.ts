import { describe, expect, it } from 'vitest';
import type { AttachmentUploadResult } from '$lib/types';
import {
	createAttachmentHostToken,
	isAttachmentSurfaceEventForHost,
	notifyAttachmentSurfaceOpen,
	registerAttachmentSurfaceListener,
	toUploadedAttachment,
	type AttachmentSurfaceOpenEvent,
	type LightboxImage,
} from './events';

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

/**
 * The unified surface channel (PLAN-2392 phase 3c-ii / TASK-2485) — the sole open
 * channel since the panel and viewer channels were retired (TASK-2490).
 *
 * Two-panes addressing shape — the bus is module-global while `ItemDetail` is
 * mounted twice — plus the three things this channel does that the retired ones
 * did not: it opens null-MIME rows (no admission gate), it enforces the index /
 * id / seed invariants at the boundary, and it deep-snapshots the set so a
 * producer that keeps mutating its own array can't reach an open surface.
 */
function surfaceEvent(over: Partial<AttachmentSurfaceOpenEvent> = {}): AttachmentSurfaceOpenEvent {
	// The default seeds match `image()`'s defaults so the consistency check is
	// satisfied unless a case deliberately diverges one.
	return {
		attachmentId: 'att-1',
		workspaceSlug: 'ws-1',
		itemId: 'item-1',
		hostToken: 'host-a',
		images: [image()],
		index: 0,
		invoker: null,
		filename: 'diagram.png',
		mime_type: 'image/png',
		size_bytes: 4096,
		...over,
	};
}

describe('isAttachmentSurfaceEventForHost', () => {
	const host = { itemId: 'item-1', hostToken: 'host-a' };

	it('matches when BOTH the item and the token are the host’s', () => {
		expect(isAttachmentSurfaceEventForHost(surfaceEvent(), host)).toBe(true);
	});

	it('ignores an event that matches only the item (the two-panes-one-item case)', () => {
		expect(isAttachmentSurfaceEventForHost(surfaceEvent({ hostToken: 'host-b' }), host)).toBe(false);
	});

	it('ignores an event that matches only the token', () => {
		expect(isAttachmentSurfaceEventForHost(surfaceEvent({ itemId: 'item-2' }), host)).toBe(false);
	});

	it('never matches on a missing half, on either side', () => {
		expect(isAttachmentSurfaceEventForHost(surfaceEvent({ hostToken: '' }), host)).toBe(false);
		expect(isAttachmentSurfaceEventForHost(surfaceEvent({ itemId: '' }), host)).toBe(false);
		expect(
			isAttachmentSurfaceEventForHost(surfaceEvent(), { itemId: 'item-1', hostToken: '' })
		).toBe(false);
		expect(isAttachmentSurfaceEventForHost(surfaceEvent(), { itemId: null, hostToken: null })).toBe(
			false
		);
	});

	it('tolerates a null/undefined event', () => {
		expect(isAttachmentSurfaceEventForHost(null, host)).toBe(false);
		expect(isAttachmentSurfaceEventForHost(undefined, host)).toBe(false);
	});
});

describe('unified surface open channel', () => {
	it('delivers to the addressed host only, with two hosts subscribed', () => {
		const master = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const peeked = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		const masterSeen: AttachmentSurfaceOpenEvent[] = [];
		const peekedSeen: AttachmentSurfaceOpenEvent[] = [];
		const offMaster = registerAttachmentSurfaceListener((e) => {
			if (isAttachmentSurfaceEventForHost(e, master)) masterSeen.push(e);
		});
		const offPeeked = registerAttachmentSurfaceListener((e) => {
			if (isAttachmentSurfaceEventForHost(e, peeked)) peekedSeen.push(e);
		});
		try {
			notifyAttachmentSurfaceOpen(surfaceEvent({ hostToken: peeked.hostToken }));
			expect(masterSeen).toHaveLength(0);
			expect(peekedSeen).toHaveLength(1);
		} finally {
			offMaster();
			offPeeked();
		}
	});

	it('carries the EMIT-TIME workspace through — never resolved from the host', () => {
		// The pane switches workspace without remounting, so the slug is the
		// emitter's, captured; a host that read it live could serve a surface
		// opened in ws1 from ws2's endpoint.
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		let received: AttachmentSurfaceOpenEvent | null = null;
		const off = registerAttachmentSurfaceListener((e) => {
			if (isAttachmentSurfaceEventForHost(e, host)) received = e;
		});
		try {
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ workspaceSlug: 'other-ws', hostToken: host.hostToken })
			);
		} finally {
			off();
		}
		const got = received as AttachmentSurfaceOpenEvent | null;
		expect(got).not.toBeNull();
		expect(got!.workspaceSlug).toBe('other-ws');
	});

	it('passes a NULL-MIME entry through — the old channel’s admission drop is gone', () => {
		// THE DEFINING DIFFERENCE from the retired image-only viewer channel, which
		// failed closed on a null `mime_type`: the converged surface opens files and
		// unresolved rows, so admission does not consult the MIME — the renderer
		// arm (`getSurfaceRenderer`) does. A null-MIME record is delivered, not
		// dropped. The flat seed is nulled to match the record (else the
		// consistency check would reject a seed disagreeing with it).
		const host = { itemId: 'item-1', hostToken: createAttachmentHostToken() };
		let received: AttachmentSurfaceOpenEvent | null = null;
		const off = registerAttachmentSurfaceListener((e) => {
			if (isAttachmentSurfaceEventForHost(e, host)) received = e;
		});
		try {
			notifyAttachmentSurfaceOpen(
				surfaceEvent({
					hostToken: host.hostToken,
					images: [image({ mime_type: null })],
					mime_type: null,
				})
			);
		} finally {
			off();
		}
		const got = received as AttachmentSurfaceOpenEvent | null;
		expect(got).not.toBeNull();
		expect(got!.images).toHaveLength(1);
		expect(got!.images[0].mime_type).toBeNull();
	});

	it('rejects an OUT-OF-RANGE index', () => {
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			// One-element set, index past the end.
			notifyAttachmentSurfaceOpen(surfaceEvent({ index: 5 }));
			// ...and a negative index.
			notifyAttachmentSurfaceOpen(surfaceEvent({ index: -1 }));
			expect(seen).toHaveLength(0);
		} finally {
			off();
		}
	});

	it('rejects when images[index].id !== attachmentId', () => {
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			// The event opens on att-2, but the record at that index is att-1 — the
			// surface would open on a different image than the one activated.
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ attachmentId: 'att-2', images: [image({ id: 'att-1' })], index: 0 })
			);
			expect(seen).toHaveLength(0);
		} finally {
			off();
		}
	});

	it('rejects a flat seed inconsistent with images[index]', () => {
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			// Record says image/png; the flat seed claims application/pdf. A
			// contradiction is a producer bug, not two captions to reconcile.
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ images: [image({ mime_type: 'image/png' })], mime_type: 'application/pdf' })
			);
			expect(seen).toHaveLength(0);
			// A NULL seed asserts nothing — the same record with the seed nulled is
			// delivered, proving it was the CONTRADICTION that was refused, not the
			// presence of a record MIME.
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ images: [image({ mime_type: 'image/png' })], mime_type: null })
			);
			expect(seen).toHaveLength(1);
		} finally {
			off();
		}
	});

	it('DEEP-snapshots the set — mutating the caller’s array or a record after notify is inert', () => {
		// The convergence point every producer funnels through, and some hand over
		// a set they still own and mutate. A shallow array copy is not enough: the
		// records stay shared references until each is spread.
		const record = image({ alt: 'original alt' });
		const images = [record];
		let received: AttachmentSurfaceOpenEvent | null = null;
		const off = registerAttachmentSurfaceListener((e) => (received = e));
		try {
			notifyAttachmentSurfaceOpen(surfaceEvent({ images }));
		} finally {
			off();
		}
		const got = received as AttachmentSurfaceOpenEvent | null;
		expect(got).not.toBeNull();
		// Mutate the caller's ARRAY (push another entry)...
		images.push(image({ id: 'att-2', alt: 'appended' }));
		// ...and mutate the caller's RECORD in place.
		record.alt = 'mutated after notify';
		// The delivered event saw neither.
		expect(got!.images).toHaveLength(1);
		expect(got!.images[0].alt).toBe('original alt');
	});

	it('rejects a malformed record anywhere in the set, not just at the index', () => {
		// A garbage NON-TARGET entry (a bare `{}`, a wrong-typed field) would
		// snapshot to a record with missing/undefined fields and reach a host that
		// pages onto it. The whole emission is refused.
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ images: [image(), {} as LightboxImage], index: 0 })
			);
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ images: [image(), { ...image({ id: 'att-2' }), alt: 42 as unknown as string }] })
			);
			// A field holding an OBJECT (would stay a shared reference under a spread).
			notifyAttachmentSurfaceOpen(
				surfaceEvent({ images: [{ ...image(), width: {} as unknown as number }] })
			);
			expect(seen).toHaveLength(0);
		} finally {
			off();
		}
	});

	it('snapshots by projection — the delivered record has ONLY the declared fields', () => {
		// Explicit field projection, not a spread: a stray property on the caller's
		// record (or a prototype-backed value) does not ride along.
		const dirty = { ...image(), sneaky: 'extra' } as unknown as LightboxImage;
		let received: AttachmentSurfaceOpenEvent | null = null;
		const off = registerAttachmentSurfaceListener((e) => (received = e));
		try {
			notifyAttachmentSurfaceOpen(surfaceEvent({ images: [dirty] }));
		} finally {
			off();
		}
		const got = received as AttachmentSurfaceOpenEvent | null;
		expect(got).not.toBeNull();
		expect(Object.keys(got!.images[0]).sort()).toEqual(
			['alt', 'filename', 'height', 'id', 'mime_type', 'size_bytes', 'width'].sort()
		);
		expect((got!.images[0] as unknown as Record<string, unknown>).sneaky).toBeUndefined();
	});

	it('projects the EVENT too — a stray top-level property does not ride along', () => {
		const dirty = { ...surfaceEvent(), sneaky: 'extra' } as unknown as AttachmentSurfaceOpenEvent;
		let received: AttachmentSurfaceOpenEvent | null = null;
		const off = registerAttachmentSurfaceListener((e) => (received = e));
		try {
			notifyAttachmentSurfaceOpen(dirty);
		} finally {
			off();
		}
		const got = received as AttachmentSurfaceOpenEvent | null;
		expect(got).not.toBeNull();
		expect((got as unknown as Record<string, unknown>).sneaky).toBeUndefined();
		// An absent (undefined) seed is normalized to null in the delivered event.
		expect(got!.filename).not.toBeUndefined();
	});

	it('treats an ABSENT (undefined) flat seed as not-asserting, like null', () => {
		// The seed fields are `string | null`; a JS caller that passes `undefined`
		// (or omits one) must be treated as "no seed", not as a value that
		// contradicts the record. The runtime half of the contract.
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			notifyAttachmentSurfaceOpen(
				surfaceEvent({
					images: [image({ mime_type: 'image/png' })],
					mime_type: undefined as unknown as string | null,
				})
			);
			expect(seen).toHaveLength(1);
		} finally {
			off();
		}
	});

	it('drops an unaddressable, workspaceless, or empty emission rather than broadcasting it', () => {
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		try {
			notifyAttachmentSurfaceOpen(surfaceEvent({ hostToken: '' }));
			notifyAttachmentSurfaceOpen(surfaceEvent({ itemId: '' }));
			notifyAttachmentSurfaceOpen(surfaceEvent({ attachmentId: '' }));
			notifyAttachmentSurfaceOpen(surfaceEvent({ workspaceSlug: '' }));
			notifyAttachmentSurfaceOpen(surfaceEvent({ images: [] }));
			// A sparse / malformed set must not snapshot to garbage and reach a host.
			notifyAttachmentSurfaceOpen(surfaceEvent({ images: new Array(1) as LightboxImage[] }));
			expect(seen).toHaveLength(0);
		} finally {
			off();
		}
	});

	it('returns a disposer that stops delivery', () => {
		const seen: AttachmentSurfaceOpenEvent[] = [];
		const off = registerAttachmentSurfaceListener((e) => seen.push(e));
		notifyAttachmentSurfaceOpen(surfaceEvent());
		expect(seen).toHaveLength(1);
		off();
		notifyAttachmentSurfaceOpen(surfaceEvent());
		expect(seen).toHaveLength(1);
	});
});
