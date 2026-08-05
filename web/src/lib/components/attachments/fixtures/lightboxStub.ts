import type { LightboxImage } from '$lib/attachments/events';

/**
 * Recording surface for `LightboxStub.svelte` (TASK-2428).
 *
 * Exists so a test can hold a viewer's `onClose` AFTER that viewer has been
 * destroyed — the only way to drive the stale-continuation case, since a click
 * on a detached button never reaches Svelte's delegated root handler and so
 * proves nothing (Codex round 4 found the click-based version vacuous).
 */
export interface LightboxStubCall {
	/**
	 * The FULL records, not `{id}` (TASK-2431): the metadata a producer threads
	 * onto each image — `mime_type` above all — is part of what it must get
	 * right, and a narrower type here would make that unassertable.
	 */
	images: LightboxImage[];
	index: number;
	wsSlug: string;
	/** Threaded down by the host since TASK-2429; the viewer owns the restore. */
	invoker: HTMLElement | null;
	onClose: () => void;
}

export const lightboxStubCalls: LightboxStubCall[] = [];
