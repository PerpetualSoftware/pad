/**
 * Recording surface for `LightboxStub.svelte` (TASK-2428).
 *
 * Exists so a test can hold a viewer's `onClose` AFTER that viewer has been
 * destroyed — the only way to drive the stale-continuation case, since a click
 * on a detached button never reaches Svelte's delegated root handler and so
 * proves nothing (Codex round 4 found the click-based version vacuous).
 */
export interface LightboxStubCall {
	images: { id: string }[];
	index: number;
	wsSlug: string;
	onClose: () => void;
}

export const lightboxStubCalls: LightboxStubCall[] = [];
