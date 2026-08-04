/**
 * The address a Tiptap attachment NodeView stamps on the events it emits
 * (PLAN-2392 DR-8), and why it is a FUNCTION rather than two strings.
 *
 * DR-8 needs two facts at emit time: which item the editor is editing, and
 * which `ItemDetail` mount owns it (a master pane and a peeked pane are both
 * mounted, so `itemId` alone would let both hosts consume one NodeView's
 * event). The obvious shape is two string options set at `configure()` time.
 *
 * That shape is a trap here, for two independent reasons:
 *
 *  1. **The comment composer outlives the item.** `CommentEditor` is
 *     deliberately reused across a no-`{#key}` item switch — its `itemId` prop
 *     just changes — so a value captured when its extensions were configured
 *     goes stale, and its chips would emit events addressed to the PREVIOUS
 *     item. The host matches on both fields and would correctly ignore them:
 *     a tap that silently does nothing.
 *
 *  2. **You cannot fix that by writing to the options.** Tiptap's `options` is
 *     a GETTER that returns a fresh spread on every access
 *     (`@tiptap/core@3.22.5`, `dist/index.cjs:3452`), so `ext.options.itemId =
 *     next` mutates a temporary that is discarded on the next line. The
 *     assignment looks like it works and does nothing. (`optionsAreASnapshot`
 *     in the sibling test pins this, so a future Tiptap bump that changes it
 *     is a visible test failure rather than a silent invitation to go back to
 *     mutating.)
 *
 * So the option is a reader the host supplies once and keeps honest: a closure
 * over its own live props. Called at emit time, it is always current, for a
 * remounted host (the body editor, re-keyed per item) and a reused one (the
 * composer) alike — one shape, no per-host special case.
 */

export interface AttachmentHostAddress {
	/** UUID of the item being edited. Empty when there is no item context. */
	itemId: string;
	/** Identity of the `ItemDetail` mount that owns this editor. */
	hostToken: string;
}

/** Reads the CURRENT address. Called at emit time, never cached by callers. */
export type AttachmentHostAddressReader = () => AttachmentHostAddress;

/** The no-context address: an editor with no host cannot address a panel. */
export const UNADDRESSED: AttachmentHostAddress = { itemId: '', hostToken: '' };

/** Default option value — an editor mounted without a host addresses nothing. */
export const readUnaddressed: AttachmentHostAddressReader = () => UNADDRESSED;

/**
 * Whether an address can reach a host at all. Both halves are required: a
 * missing token would make the event ambiguous between concurrently-mounted
 * hosts, which is the exact failure DR-8 exists to prevent.
 */
export function isAddressable(address: AttachmentHostAddress | null | undefined): boolean {
	return Boolean(address?.itemId && address?.hostToken);
}
