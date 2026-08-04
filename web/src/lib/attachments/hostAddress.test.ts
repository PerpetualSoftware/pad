// Host addressing for attachment NodeViews (PLAN-2392 DR-8, TASK-2421).
//
// The interesting assertion in this file is the LAST one. The address is a
// reader instead of two string options because Tiptap's `options` is a getter
// returning a fresh spread per access — writing to it after configure() is a
// no-op that looks exactly like working code. That is a property of a
// dependency, so it is pinned here: if a future @tiptap/core bump makes
// options writable, this test fails and someone re-reads the reasoning
// instead of discovering it through a chip that silently does nothing.
import { describe, it, expect } from 'vitest';
import { AttachmentChip } from '$lib/components/editor/attachment-chip';
import {
	isAddressable,
	readUnaddressed,
	type AttachmentHostAddress,
	type AttachmentHostAddressReader,
} from './hostAddress';

describe('attachment host address', () => {
	it('reads through to the host live, so a reused editor re-addresses on an item switch', () => {
		// Exactly the composer's situation: the component instance survives an
		// A→B item switch and its `itemId` prop just changes underneath.
		let itemId = 'item-A';
		const hostToken = 'apanel-1';
		const read: AttachmentHostAddressReader = () => ({ itemId, hostToken });

		expect(read()).toEqual({ itemId: 'item-A', hostToken: 'apanel-1' });
		itemId = 'item-B';
		expect(read()).toEqual({ itemId: 'item-B', hostToken: 'apanel-1' });
	});

	it('treats a half-address as unaddressable, in both directions', () => {
		// A token without an item, or an item without a token, cannot pick out
		// ONE of two concurrently-mounted hosts — which is the whole job.
		expect(isAddressable({ itemId: 'item-A', hostToken: 'apanel-1' })).toBe(true);
		expect(isAddressable({ itemId: 'item-A', hostToken: '' })).toBe(false);
		expect(isAddressable({ itemId: '', hostToken: 'apanel-1' })).toBe(false);
		expect(isAddressable(readUnaddressed())).toBe(false);
		expect(isAddressable(null)).toBe(false);
	});

	it('defaults to unaddressed, so an editor with no host broadcasts to nobody', () => {
		const ext = AttachmentChip.configure({});
		expect(isAddressable(ext.options.address())).toBe(false);
	});

	it('carries the configured reader through to the extension options', () => {
		const address: AttachmentHostAddress = { itemId: 'item-A', hostToken: 'apanel-1' };
		const ext = AttachmentChip.configure({ address: () => address });
		expect(ext.options.address()).toEqual(address);

		// And it stays live: the extension holds the reader, not a snapshot.
		address.itemId = 'item-B';
		expect(ext.options.address().itemId).toBe('item-B');
	});

	it('pins the reason this is a reader: Tiptap options are a per-access snapshot', () => {
		const ext = AttachmentChip.configure({ workspaceSlug: 'ws' });

		// Each read builds a new object...
		expect(ext.options).not.toBe(ext.options);
		// ...so assigning to one is discarded, silently.
		ext.options.workspaceSlug = 'clobbered';
		expect(ext.options.workspaceSlug).toBe('ws');
	});
});
