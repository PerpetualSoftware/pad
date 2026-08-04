import { describe, expect, it } from 'vitest';
import {
	createAttachmentHostToken,
	isAttachmentPanelEventForHost,
	notifyAttachmentPanelOpen,
	registerAttachmentPanelListener,
	type AttachmentPanelOpenEvent,
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
