/**
 * Attachment actions — defined once, rendered by whoever needs them
 * (PLAN-2392 DR-5).
 *
 * "The panel and the viewer share one action list" is a promise with no source
 * of truth unless the list IS the source of truth. So the actions live here as
 * descriptors: a surface renders them, none of them owns the set, and adding an
 * action means adding one descriptor here.
 *
 * TODAY THERE IS ONE CONSUMER: the options panel, which draws them as a
 * menu/sheet. The unified image viewer — the second renderer, an inline
 * toolbar over the same list — arrives in phase 3a, which is also what will
 * consume the `address` option now threaded onto the image NodeView. Stated
 * plainly because "rendered twice" read as a description of the present and
 * was not one; a list with a single consumer is a shape held open on purpose,
 * and worth re-justifying if 3a ever stops coming.
 *
 * TWO DESCRIPTOR SHAPES, not one, because the ELEMENT is part of the contract
 * (DR-5, round 35/36):
 *
 *  - `element: 'anchor'` — Download must remain a real `<a download>`: the
 *    server sends `Content-Disposition: inline` for most accepted types
 *    (`handlers_attachments.go:742`), so a plain navigation would *view* the
 *    file rather than save it (DR-16). Open needs anchor semantics too — new
 *    tab, middle-click, "copy link address". An anchor descriptor supplies
 *    `href(ctx)` and optionally `download(ctx)` / `target` / `rel`; the
 *    browser performs the action, so there is deliberately no `run()` to call.
 *    (Renderers that called both would fire the action twice.)
 *  - `element: 'button'` — Copy link and Delete do work in JS, so they carry
 *    `run(ctx)` and no `href`.
 *
 * The union is discriminated on `element`, so a renderer that switches on it
 * gets `href` or `run` narrowed for free and cannot reach for the wrong one.
 *
 * "Can the browser preview this?" is answered by importing `canBrowserPreview`
 * from the display helpers, NOT by taking a predicate from the caller. DR-16
 * puts every "what can this MIME do" question in one module precisely so a
 * single call site cannot be given a looser answer — an injected predicate
 * that admitted `image/svg+xml` would reopen the hole the exact-allowlist
 * decision exists to close. (It was injected while this module and the
 * predicate were built in parallel; collapsed at integration.)
 *
 * Not here, deliberately: `state_generation` and Undo. This plan's delete
 * behaves exactly like today's tile delete; the generation token, the event
 * payload change and the Undo toast arrive together in PLAN-2411 across all
 * three entry points at once (DR-19).
 */

import { api } from '$lib/api/client';
import { announceAttachmentDeleted } from '$lib/attachments/events';
import { canBrowserPreview } from '$lib/attachments/display';
import { copyToClipboard } from '$lib/utils/clipboard';

export type AttachmentActionId = 'open' | 'download' | 'copy-link' | 'delete';

/**
 * The attachment an action acts on. Deliberately the three fields every
 * surface already has (strip tile, panel, viewer) rather than a full
 * `Attachment` row — the viewer's image shape is not the list row's.
 */
export interface AttachmentActionSubject {
	id: string;
	filename: string;
	mime_type: string;
}

export interface AttachmentActionContext {
	workspaceSlug: string;
	attachment: AttachmentActionSubject;
	/**
	 * Whether the caller may mutate — a read-only share view, a viewer role,
	 * or a pane whose mutation gate is closed. Delete is disabled without it.
	 */
	mutationsEnabled: boolean;
	/**
	 * Origin for the absolute copy-link URL. Defaults to `location.origin`;
	 * present so the URL builder is testable outside a DOM.
	 */
	origin?: string;
	/**
	 * Confirmation gate for Delete. The surface owns the modality, so a
	 * descriptor never invents one — but when supplied, returning false aborts
	 * before any request is sent. Every surface now resolves this through the
	 * shared `AttachmentDeleteConfirm` drill-down (DR-18); the native
	 * `window.confirm` the strip used to raise is gone.
	 */
	confirmDelete?: (attachment: AttachmentActionSubject) => boolean | Promise<boolean>;
	/** Called after the server confirms the row is gone (204 or 404). */
	onDeleted?: (attachmentId: string) => void;
	/** Called with the copied URL after a successful clipboard write. */
	onCopied?: (url: string) => void;
	/** Clipboard override — the LAN/HTTP-safe `copyToClipboard` by default. */
	copyText?: (text: string) => Promise<boolean>;
}

interface BaseAttachmentAction {
	id: AttachmentActionId;
	/** Row/button label. Visible text, so it carries the honest semantics. */
	label: string;
	/** Leading glyph, in `MenuItem`'s string-icon vocabulary. */
	icon: string;
	/** Longer explanation for a tooltip / sublabel. */
	description?: string;
	/**
	 * Whether the action EXISTS for this attachment. Open is omitted entirely
	 * for types a browser cannot preview — never shown disabled, because a
	 * greyed "Open" implies a preview Pad could give and won't (DR-5).
	 */
	applies(ctx: AttachmentActionContext): boolean;
	/** Whether the action is currently actionable. */
	enabled(ctx: AttachmentActionContext): boolean;
}

export interface AnchorAttachmentAction extends BaseAttachmentAction {
	element: 'anchor';
	href(ctx: AttachmentActionContext): string;
	/** `download` attribute value — the filename, or undefined for none. */
	download?(ctx: AttachmentActionContext): string | undefined;
	target?: string;
	rel?: string;
}

export interface ButtonAttachmentAction extends BaseAttachmentAction {
	element: 'button';
	/** Destructive styling (red row). */
	danger?: boolean;
	run(ctx: AttachmentActionContext): Promise<void>;
}

export type AttachmentAction = AnchorAttachmentAction | ButtonAttachmentAction;

/**
 * The absolute, same-origin URL for an attachment (DR-5a).
 *
 * `api.attachments.downloadUrl` returns a RELATIVE `/api/v1/...` path
 * (`api/client.ts:2128`), so copying it verbatim yields something that does
 * not work anywhere it gets pasted. It is still not a share link: the endpoint
 * requires the recipient's own authenticated workspace access, and a recipient
 * without it gets the normal auth redirect.
 */
export function attachmentLinkUrl(ctx: AttachmentActionContext): string {
	const path = api.attachments.downloadUrl(ctx.workspaceSlug, ctx.attachment.id);
	const origin = ctx.origin ?? (typeof location !== 'undefined' ? location.origin : '');
	return `${origin}${path}`;
}

/** Both anchors need a workspace and an id before they can point anywhere. */
function addressable(ctx: AttachmentActionContext): boolean {
	return Boolean(ctx.workspaceSlug && ctx.attachment?.id);
}

function errorCode(err: unknown): string | null {
	if (err && typeof err === 'object' && 'code' in err) {
		const code = (err as { code?: unknown }).code;
		return typeof code === 'string' ? code : null;
	}
	return null;
}

export const ATTACHMENT_ACTIONS: readonly AttachmentAction[] = [
	{
		id: 'open',
		label: 'Open in new tab',
		icon: '⇗',
		description: 'Hands the file to the browser to preview.',
		element: 'anchor',
		// Only for what a browser previews natively. Never a Pad-rendered
		// preview, and never offered for a .zip or an office document —
		// those get Download only.
		applies: (ctx) => canBrowserPreview(ctx.attachment.mime_type),
		enabled: addressable,
		href: (ctx) => api.attachments.downloadUrl(ctx.workspaceSlug, ctx.attachment.id),
		target: '_blank',
		rel: 'noopener noreferrer',
	} satisfies AnchorAttachmentAction,
	{
		id: 'download',
		label: 'Download',
		icon: '⇩',
		element: 'anchor',
		applies: () => true,
		enabled: addressable,
		href: (ctx) => api.attachments.downloadUrl(ctx.workspaceSlug, ctx.attachment.id),
		// A REAL download attribute, not decoration: without it the inline
		// disposition the server sends for most types would open the file
		// instead of saving it (DR-16).
		download: (ctx) => ctx.attachment.filename || undefined,
	} satisfies AnchorAttachmentAction,
	{
		id: 'copy-link',
		// Honest by name (DR-5a): this is a link for people who already have
		// access to this workspace, not a public share link. Pad has no
		// attachment share token.
		label: 'Copy workspace link',
		icon: '🔗',
		description: 'Opens only for people with access to this workspace.',
		element: 'button',
		applies: () => true,
		enabled: addressable,
		async run(ctx) {
			const url = attachmentLinkUrl(ctx);
			const copy = ctx.copyText ?? copyToClipboard;
			const ok = await copy(url);
			if (!ok) throw new Error('Could not copy the link to the clipboard');
			ctx.onCopied?.(url);
		},
	} satisfies ButtonAttachmentAction,
	{
		id: 'delete',
		label: 'Delete',
		icon: '🗑',
		element: 'button',
		danger: true,
		applies: () => true,
		enabled: (ctx) => ctx.mutationsEnabled && addressable(ctx),
		async run(ctx) {
			// Belt and braces: a renderer that draws a disabled row can still
			// be asked to run it by a stray keyboard activation.
			if (!ctx.mutationsEnabled || !addressable(ctx)) return;

			// Snapshot identity BEFORE the confirmation, not after. `ctx` may be
			// a live object owned by a surface that survives an item switch (the
			// no-{#key} pane is built around exactly that), and `confirmDelete`
			// is allowed to be async — an in-app confirmation is a whole UI
			// interaction, so the user has all the time in the world to switch
			// items or lose their mutation rights while it is up. Reading
			// `ctx.attachment.id` after the await could name a DIFFERENT
			// attachment than the one the user was shown.
			const ws = ctx.workspaceSlug;
			const id = ctx.attachment.id;
			const subject = { ...ctx.attachment };

			if (ctx.confirmDelete && !(await ctx.confirmDelete(subject))) return;

			// Re-check the gate on the way out of the confirmation: permission
			// can be revoked while it is open (a pane being peeked closes the
			// mutation gate), and identity must still agree with what was
			// confirmed.
			if (!ctx.mutationsEnabled) return;
			if (ctx.workspaceSlug !== ws || ctx.attachment.id !== id) return;
			try {
				await api.attachments.delete(ws, id);
			} catch (err) {
				// A 404 is just as authoritative as a 204 about the row being
				// gone (another tab, another user), so it gets the same
				// reconciliation rather than being surfaced as a failure —
				// exactly what the strip's tile delete does today.
				if (errorCode(err) === 'not_found') {
					announceAttachmentDeleted(ws, id);
					ctx.onDeleted?.(id);
					return;
				}
				throw err;
			}
			// Tell the live views and drop the cached HEAD metadata. An <img>
			// that already painted never re-requests, so without this the body
			// keeps showing an image the server no longer has.
			announceAttachmentDeleted(ws, id);
			ctx.onDeleted?.(id);
		},
	} satisfies ButtonAttachmentAction,
];

/**
 * The actions that exist for this attachment, in render order. Actions that
 * don't apply are absent, not disabled; actions that apply but aren't
 * currently actionable come back with `enabled(ctx) === false` so the renderer
 * can grey them.
 */
export function attachmentActionsFor(ctx: AttachmentActionContext): AttachmentAction[] {
	return ATTACHMENT_ACTIONS.filter((action) => action.applies(ctx));
}
