<!--
@component
CopyItemDialog — the cross-workspace copy / move review dialog (PLAN-2373 /
TASK-2355, backend contract PLAN-2357).

Built on the shared `Modal` primitive (native `<dialog>`) rather than the pane
menu's drill-down pattern: the field-mapping step needs more room than the menu
affords, and the native dialog gives focus trap + Escape + top-layer rendering,
which PaneHost's mobile focus trap deliberately defers to. Consumer obligations
from Modal's docs are honored: it is NOT wrapped in `{#if open}`, and
`labelledby` points at the heading.

Two submit paths behind one dialog (DR-18):

  - destination workspace == the source workspace → `onmove()`, which delegates
    to the page's existing `handleMove` (POST /items/{slug}/move). That path
    keeps its open-children force retry and its `navIfStillCurrent` route
    identity fencing. The copy endpoint has NO same-workspace guard, so routing
    a same-workspace destination through it would mint a NEW item id, drop the
    parent and clone attachments instead of relocating the item — and since
    this destination presents as Move, an `archive_source` matching that label
    would archive the original on top. A silent behaviour regression on an
    existing surface either way. Same-workspace *copy* (duplicate in place) is
    deliberately out of scope, so a same-workspace destination locks the toggle
    to Move.
  - destination workspace != the source workspace → the preflight + copy
    endpoints, with `archive_source` on the move path.

The preflight drives BOTH paths' field mapping, which is what finally closes
the intra-workspace "Required fields missing: …" dead end — the old move call
site passed `field_overrides: undefined` and turned the server's refusal into
an unactionable toast.

Ambiguity rule (DR-13 / round-10 amendment). Ambiguity is decided by "was the
mutation dispatched?", NOT by the server returning `copy_failed`. Once the copy
POST is dispatched, any UNSTRUCTURED failure (rejected fetch, non-JSON 502,
timeout) is as ambiguous to the user as an explicit `copy_failed`: the server
may have committed. Both render the same outcome-unknown state, which
PROHIBITS retry (there is no idempotency key) and sends the user to look at the
destination. Structured PRE-WRITE refusals — `collection_not_found`, a
destination `forbidden`, `plan_limit_exceeded`, the validation family — keep
their own recovery paths; swallowing those into outcome-unknown would send the
user hunting for an item that provably does not exist.
-->
<script lang="ts">
	import { tick, untrack } from 'svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import { api, PadApiError } from '$lib/api/client';
	import { canEditCollection } from '$lib/utils/permissions';
	import { parseSchema } from '$lib/types';
	import type {
		Collection,
		FieldDef,
		Item,
		ItemCopyPreflight,
		ItemCopyPreflightNeedsValue,
		ItemCopyPreflightRequest,
		ItemCopyResult,
		Workspace,
		WorkspaceMembership
	} from '$lib/types';

	interface Props {
		open: boolean;
		/** Dismissal request from Escape / backdrop / Cancel / a completed submit.
		 *  The parent flips `open` and restores focus to the ⋯ trigger. */
		onclose: () => void;
		sourceWsSlug: string;
		/** The item's ACTUAL collection slug, not the route's. */
		sourceCollectionSlug: string;
		item: Item;
		/** Human ref for the heading, e.g. "TASK-14". */
		sourceRef: string;
		/**
		 * True when the source item can no longer be copied — archived or
		 * deleted, including a remote archive/delete that arrived over SSE while
		 * this dialog was open. Disables Confirm immediately.
		 */
		sourceUnavailable?: boolean;
		/**
		 * Foreground-flush the live collab editor into `items.content` and resolve
		 * false if it FAILED. `items.content` can lag the authoritative Y.Doc by
		 * up to the 5s flush debounce, and copying content the user just typed but
		 * that never reached the server is the worst outcome available here — so a
		 * failed flush BLOCKS the copy.
		 */
		flushContent: () => Promise<boolean>;
		/** Same-workspace destination: delegate to the page's `handleMove`. */
		onmove: (
			targetCollection: string,
			fieldOverrides: Record<string, unknown>
		) => Promise<{ status: 'ok' | 'cancelled' | 'failed'; message?: string }>;
		/** Cross-workspace success. The parent owns navigation / toasts. */
		oncopied: (result: ItemCopyResult) => void;
	}

	let {
		open,
		onclose,
		sourceWsSlug,
		sourceCollectionSlug,
		item,
		sourceRef,
		sourceUnavailable = false,
		flushContent,
		onmove,
		oncopied
	}: Props = $props();

	const uid = $props.id();
	const titleId = `copy-dialog-title-${uid}`;

	// ── Preflight timing ──────────────────────────────────────────────────
	// Typed overrides re-run the preflight, which rescans relationships and
	// replans attachments server-side — real work, on a rate-limited endpoint.
	// So: a debounce, PLUS single-flight trailing coalescing (round-12 fold-in).
	// At most one preflight is in flight at any moment; edits that settle while
	// one is running collapse into exactly ONE trailing run.
	const PREFLIGHT_DEBOUNCE_MS = 250;

	/**
	 * Types the dialog can safely collect a value for — the set `FieldEditor`
	 * implements as real typed controls.
	 *
	 * Everything else is NOT rendered as a text box. `json` is deliberately
	 * read-only in FieldEditor (a plain text input would store the string
	 * "[]" where an array belongs), `relation` has no picker here, and
	 * `multi_select` falls through FieldEditor's TEXT fallback — which yields a
	 * string where the server's validation requires `[]any`. That last one is
	 * the dangerous case: enterable and silently invalid. So a required field of
	 * any uncollectable type renders an explicit blocked state naming the field
	 * and its type, rather than a dead Confirm or a lying input.
	 */
	const COLLECTABLE_TYPES = new Set(['text', 'number', 'select', 'date', 'checkbox', 'url']);

	// ── Destination selection ─────────────────────────────────────────────
	let workspaces = $state<Workspace[]>([]);
	let workspacesLoading = $state(false);
	let workspacesError = $state('');

	let destWs = $state('');
	let destColl = $state('');

	let destCollections = $state<Collection[]>([]);
	let destCollectionsLoading = $state(false);
	let destCollectionsError = $state('');

	let mode = $state<'copy' | 'move'>('copy');

	// ── Preview ───────────────────────────────────────────────────────────
	let preflight = $state<ItemCopyPreflight | null>(null);
	let preflightLoading = $state(false);
	let preflightError = $state('');

	/**
	 * Sticky override rows. A field the user has satisfied leaves the NEXT
	 * preflight's `needs_value` bucket — if the inputs were rendered straight
	 * from that bucket, the control would vanish (and take focus with it) the
	 * moment it was filled in. So rows accumulate here per destination
	 * collection and are cleared when the destination changes.
	 */
	let overrideRows = $state<ItemCopyPreflightNeedsValue[]>([]);
	/**
	 * What is actually SENT. Restricted to keys the CURRENT destination's schema
	 * DECLARES — the server rejects an override naming an undeclared key with a
	 * 400 `malformed_override`, so retained values from a previous destination
	 * must not be sent blind.
	 */
	let overrides = $state<Record<string, unknown>>({});
	/**
	 * Everything the user has typed, kept across destination changes (round-8
	 * amendment: entered values are still valid input for a replacement
	 * destination). Re-applied to `overrides` only once the new destination's
	 * preflight confirms it declares the key.
	 */
	let retainedValues = $state<Record<string, unknown>>({});

	// ── Submission ────────────────────────────────────────────────────────
	/** Pre-dispatch work (collab flush + the final preflight). Disables Confirm
	 *  but does NOT veto dismissal — nothing has been dispatched yet. */
	let preparing = $state(false);
	/** Set once the mutation is DISPATCHED. Vetoes Escape / backdrop / Cancel:
	 *  dismissal is not rollback, and the copy commits regardless. */
	let submitting = $state(false);
	let submitError = $state('');
	/** Outcome unknown — the copy may or may not have committed. Terminal:
	 *  Confirm stays locked, and no retry is offered (DR-13). */
	let outcomeUnknown = $state('');
	/** The preview changed between review and submit — advisory only (see
	 *  `reviewFingerprint`). Requires an explicit re-confirmation. */
	let staleReview = $state('');
	/** The destination went away (soft-deleted collection / revoked access) —
	 *  a PRE-WRITE refusal, so it must NOT read as outcome-unknown. */
	let destinationLost = $state('');

	/**
	 * DIALOG-level fence: bumped on open, on close, and on a destination
	 * WORKSPACE change — i.e. whenever the in-flight workspace/collection/
	 * membership loads become irrelevant. Every await-then-write re-checks it:
	 * this dialog lives inside `ItemDetail`, a persistent pane with no `{#key}`,
	 * so a late continuation could otherwise write into a dialog the user has
	 * already re-pointed or closed.
	 */
	let flowGen = 0;

	/**
	 * PREVIEW-level fence: bumped by anything that invalidates the preview —
	 * workspace, collection, or copy/move mode. Separate from `flowGen` on
	 * purpose: a collection or mode change must discard an in-flight preflight
	 * WITHOUT stranding an unrelated in-flight collections/membership load
	 * behind a generation it can no longer match.
	 *
	 * Without it, a preflight for collection A could resolve after the user
	 * picked B and merge A's required-field rows into B's sticky row set —
	 * inviting an override naming a key B does not declare, which the server
	 * rejects with 400 `malformed_override`.
	 */
	let previewGen = 0;
	// Bumped by every override edit. SEPARATE from previewGen on purpose:
	// previewGen cancels in-flight preflights, and an override edit must NOT do
	// that — the debounce + single-flight runner already collapse rapid edits.
	// What an override edit MUST do is supersede an in-progress confirm, whose
	// request was built from the OLD values (final-review P1).
	let overrideGen = 0;

	// ── Derived ───────────────────────────────────────────────────────────

	let crossWorkspace = $derived(!!destWs && destWs !== sourceWsSlug);

	/**
	 * The action that will actually run. A same-workspace destination is LOCKED
	 * to Move — same-workspace copy is out of scope, and this is a `$derived`
	 * rather than an `$effect` writing `mode` so the conversion is atomic with
	 * the destination change: there is no frame in which Confirm is enabled
	 * while the UI still says "Copy".
	 */
	let effectiveMode = $derived<'copy' | 'move'>(crossWorkspace ? mode : 'move');
	let archiveSource = $derived(crossWorkspace && effectiveMode === 'move');

	let destWorkspaceName = $derived(
		workspaces.find((w) => w.slug === destWs)?.name ?? destWs
	);

	/** Required destination fields the dialog cannot safely collect a value for. */
	let blockedFields = $derived(
		(preflight?.fields.needs_value ?? []).filter(
			(f) => !COLLECTABLE_TYPES.has(f.type ?? 'text')
		)
	);

	let warnings = $derived(preflight?.warnings ?? null);

	let linkRows = $derived(
		warnings
			? [
					...Object.entries(warnings.outgoing_links).map(([t, n]) => ({
						dir: 'outgoing' as const,
						type: t,
						count: n
					})),
					...Object.entries(warnings.incoming_links).map(([t, n]) => ({
						dir: 'incoming' as const,
						type: t,
						count: n
					}))
				].filter((r) => r.count > 0)
			: []
	);

	let canConfirm = $derived(
		!!destWs &&
			!!destColl &&
			!!preflight &&
			preflight.valid &&
			blockedFields.length === 0 &&
			!preflightLoading &&
			!preparing &&
			!submitting &&
			!outcomeUnknown &&
			!destinationLost &&
			!sourceUnavailable
	);

	let confirmLabel = $derived(
		submitting
			? effectiveMode === 'move'
				? 'Moving…'
				: 'Copying…'
			: preparing
				? 'Checking…'
				: staleReview
					? effectiveMode === 'move'
						? 'Move anyway'
						: 'Copy anyway'
					: effectiveMode === 'move'
						? 'Move'
						: 'Copy'
	);

	// ── Open / close lifecycle ────────────────────────────────────────────
	// Plain variable (NOT $state) for edge detection: a $state written inside an
	// effect that also reads it self-invalidates and wedges the scheduler in a
	// production build (CONVE-1688).
	let prevOpen = false;

	$effect.pre(() => {
		// `open` is the ONLY dependency this effect may have. Both branches are
		// untracked because they write ~18 pieces of $state and read some of
		// them back — `resetForOpen` writes `destWs` and then reads it again to
		// kick off the collection load. Tracked, that is a self-dependency: the
		// write invalidates the effect that performed it, the flush aborts, and
		// Svelte's scheduler is left wedged (CONVE-1688).
		//
		// It survived review because it only bites on the SECOND open. On the
		// first, `destWs` already equals `sourceWsSlug`, so the reset is a
		// no-op write and nothing invalidates. Change the destination once and
		// reopen, and the reset becomes a real change — the dialog silently
		// fails to open and every other control on the pane dies with it, with
		// no console error because a production build reports nothing.
		const isOpen = open;
		untrack(() => {
			if (isOpen && !prevOpen) {
				resetForOpen();
			} else if (!isOpen && prevOpen) {
				cancelPending();
			}
			prevOpen = isOpen;
		});
	});

	$effect(() => {
		return () => cancelPending();
	});

	function resetForOpen() {
		flowGen++;
		previewGen++;
		destWs = sourceWsSlug;
		destColl = '';
		mode = 'copy';
		destCollections = [];
		destCollectionsError = '';
		preflight = null;
		preflightError = '';
		overrideRows = [];
		overrides = {};
		retainedValues = {};
		preparing = false;
		submitting = false;
		submitError = '';
		outcomeUnknown = '';
		staleReview = '';
		destinationLost = '';
		void loadWorkspaces();
		void loadDestCollections(destWs);
	}

	function cancelPending() {
		flowGen++;
		invalidatePreview();
	}

	/** Discard the current preview and anything in flight for it. */
	function invalidatePreview() {
		previewGen++;
		clearTimeout(debounceTimer);
		debounceTimer = undefined;
		trailingQueued = false;
		preflightLoading = false;
		abortInFlightPreflight();
	}

	/**
	 * The single dismissal seam. `Modal` forwards Escape UNCONDITIONALLY (it
	 * preventDefaults the native close and calls `onclose`, so the parent stays
	 * the single source of truth) and there is no prop that suppresses it — so
	 * the in-flight veto has to live here. Backdrop is additionally gated by
	 * `closeOnBackdrop` below; Escape is not, which is exactly why this check
	 * exists. Dismissing is not rollback: the mutation commits regardless, and
	 * "I pressed Escape" must never read as "nothing happened".
	 */
	function handleDismiss() {
		if (submitting) return;
		cancelPending();
		onclose();
	}

	// ── Destination loading ───────────────────────────────────────────────

	async function loadWorkspaces() {
		const gen = flowGen;
		workspacesLoading = true;
		workspacesError = '';
		try {
			const list = await api.workspaces.list();
			if (gen !== flowGen) return;
			workspaces = list;
		} catch (e: any) {
			if (gen !== flowGen) return;
			workspacesError = e?.message ?? 'Could not load your workspaces.';
		} finally {
			if (gen === flowGen) workspacesLoading = false;
		}
	}

	/**
	 * Load the destination's collections and filter them through
	 * `canEditCollection` for the caller's permissions IN THAT WORKSPACE
	 * (DR-19). Nav visibility is strictly weaker than collection-create
	 * permission, which is what the server actually enforces before copying, so
	 * an unfiltered picker would offer destinations the copy will reject.
	 *
	 * The membership is fetched with `api.workspaces.me(slug)` directly rather
	 * than through `workspaceStore.setCurrent` — this pane is persistent, and
	 * pointing the ambient permission store at another workspace would leak into
	 * every other affordance on the page.
	 *
	 * Always refetched, never cached: revocation is one of the two cases the
	 * filter exists to catch.
	 */
	async function loadDestCollections(wsSlug: string) {
		const gen = flowGen;
		destCollectionsLoading = true;
		destCollectionsError = '';
		destCollections = [];
		try {
			const [colls, membership] = await Promise.all([
				api.collections.list(wsSlug),
				api.workspaces.me(wsSlug) as Promise<WorkspaceMembership | null>
			]);
			if (gen !== flowGen) return;
			destCollections = colls.filter((c) => {
				if (!canEditCollection(membership, c.id)) return false;
				// A same-workspace "move" into the item's own collection is a
				// no-op; mirror the pane menu's move-target filter.
				if (wsSlug === sourceWsSlug && c.slug === sourceCollectionSlug) return false;
				return true;
			});
		} catch (e: any) {
			if (gen !== flowGen) return;
			destCollectionsError = e?.message ?? 'Could not load collections for that workspace.';
		} finally {
			if (gen === flowGen) destCollectionsLoading = false;
		}
	}

	function handleWorkspaceChange(slug: string) {
		if (slug === destWs) return;
		// A destination change invalidates the preview and the collection
		// selection, but NOT the values the user has typed — they are still
		// valid input for the replacement destination.
		flowGen++;
		invalidatePreview();
		destWs = slug;
		destColl = '';
		preflight = null;
		preflightError = '';
		submitError = '';
		staleReview = '';
		destinationLost = '';
		overrideRows = [];
		// `overrides` is destination-scoped and must not carry blind (an
		// undeclared key is a 400); `retainedValues` survives and is re-applied
		// per key once the new destination reports it in `needs_value`.
		overrides = {};
		void loadDestCollections(slug);
	}

	/**
	 * Re-apply retained values to the keys the chosen destination DECLARES.
	 *
	 * Gating on the destination's schema rather than on its `needs_value` rows
	 * matters: a value entered for a field that is REQUIRED in destination A and
	 * merely OPTIONAL in destination B would otherwise be silently neither sent
	 * nor shown — the user's input would vanish without a word. Declared-key
	 * gating also keeps `malformed_override` off the table.
	 *
	 * A value that the destination declares but rejects (e.g. a select option it
	 * doesn't have) comes back as a `needs_value` row with reason
	 * `invalid_value` and the server's message, which is visible and fixable.
	 */
	function seedOverridesForCollection(slug: string) {
		const coll = destCollections.find((c) => c.slug === slug);
		if (!coll) {
			overrides = {};
			return;
		}
		const declared = new Set(parseSchema(coll).fields.map((f) => f.key));
		const next: Record<string, unknown> = {};
		for (const [k, v] of Object.entries(retainedValues)) {
			if (declared.has(k)) next[k] = v;
		}
		overrides = next;
	}

	function handleCollectionChange(slug: string) {
		// Fence + abort: a preflight for the PREVIOUS collection must not merge
		// its required-field rows into this one's sticky set.
		invalidatePreview();
		destColl = slug;
		preflight = null;
		preflightError = '';
		submitError = '';
		staleReview = '';
		destinationLost = '';
		overrideRows = [];
		seedOverridesForCollection(slug);
		schedulePreflight({ immediate: true });
	}

	function handleModeChange(next: 'copy' | 'move') {
		// The mode changes `archive_source`, so the in-flight preview describes
		// the other operation's consequences — discard it.
		invalidatePreview();
		mode = next;
		staleReview = '';
		submitError = '';
		schedulePreflight({ immediate: true });
	}

	function handleOverrideChange(key: string, value: unknown) {
		// NOT a preview invalidation: the rows stay, the destination is the same,
		// and the debounce + single-flight runner already collapse rapid edits.
		overrides = { ...overrides, [key]: value };
		retainedValues = { ...retainedValues, [key]: value };
		staleReview = '';
		// Supersede any confirm currently flushing/re-checking: its captured
		// request carries the pre-edit values, and dispatching it would copy
		// something the user can see is no longer what they entered.
		overrideGen++;
		schedulePreflight();
	}

	// ── Preflight runner: debounce + single-flight trailing coalescing ─────

	let debounceTimer: ReturnType<typeof setTimeout> | undefined;
	let preflightInFlight = false;
	let trailingQueued = false;
	let preflightAbort: AbortController | null = null;

	function abortInFlightPreflight() {
		preflightAbort?.abort();
		preflightAbort = null;
	}

	function schedulePreflight(opts?: { immediate?: boolean }) {
		clearTimeout(debounceTimer);
		if (opts?.immediate) {
			void runPreflight();
			return;
		}
		debounceTimer = setTimeout(() => {
			debounceTimer = undefined;
			void runPreflight();
		}, PREFLIGHT_DEBOUNCE_MS);
	}

	function buildRequest(): ItemCopyPreflightRequest {
		return {
			target_workspace: destWs,
			target_collection: destColl,
			field_overrides: Object.keys(overrides).length ? { ...overrides } : undefined,
			archive_source: archiveSource
		};
	}

	async function runPreflight() {
		if (!destWs || !destColl) {
			preflightLoading = false;
			return;
		}
		// Single-flight: at most ONE preflight is ever in flight. A request that
		// arrives while one is running sets the trailing flag, and exactly one
		// trailing run fires when the current one settles — so a burst of edits
		// across many inputs collapses to (in-flight + 1) rather than N.
		if (preflightInFlight) {
			trailingQueued = true;
			return;
		}
		const gen = flowGen;
		const pgen = previewGen;
		preflightInFlight = true;
		preflightLoading = true;
		preflightError = '';
		const ctl = new AbortController();
		preflightAbort = ctl;
		try {
			const result = await api.items.copyPreflight(sourceWsSlug, item.slug, buildRequest(), {
				signal: ctl.signal
			});
			if (gen !== flowGen || pgen !== previewGen) return;
			preflight = result;
			// A retained value that this destination turns out to declare has
			// just been applied — re-preview so the buckets reflect it. This
			// converges: the re-run's needs_value can only shrink, so no further
			// retained key can be newly applied.
			if (mergeOverrideRows(result.fields.needs_value)) trailingQueued = true;
		} catch (e: any) {
			if (gen !== flowGen || pgen !== previewGen || ctl.signal.aborted) return;
			if (isDestinationGoneError(e)) {
				handleDestinationLost();
				return;
			}
			// The destination schema drifted between the collection list fetch and
			// this preview, so a seeded override names a key it no longer
			// declares. Drop the seeds and re-preview once rather than dead-ending
			// on an error the user never caused; the values stay in
			// `retainedValues` and reappear if the destination asks for them.
			if (errorCode(e) === 'malformed_override' && Object.keys(overrides).length > 0) {
				overrides = {};
				trailingQueued = true;
				return;
			}
			preflight = null;
			preflightError = e?.message ?? 'Could not preview the copy.';
		} finally {
			if (preflightAbort === ctl) preflightAbort = null;
			preflightInFlight = false;
			// The trailing run re-reads the CURRENT destination, so it fires
			// regardless of which generation this settling run belonged to — and
			// it owns `preflightLoading` from here. Clearing the flag in the
			// no-trailing branch only is what keeps a superseded run from
			// stranding the spinner (or the Confirm gate) on.
			if (trailingQueued) {
				trailingQueued = false;
				void runPreflight();
			} else {
				preflightLoading = false;
			}
		}
	}

	/**
	 * Accumulate newly-reported needs_value rows without dropping satisfied
	 * ones, and re-apply any retained value now that this destination has been
	 * confirmed to declare the key. Returns true when a retained value was
	 * newly applied, so the caller can re-preview with it.
	 */
	function mergeOverrideRows(rows: ItemCopyPreflightNeedsValue[]): boolean {
		const byKey = new Map(overrideRows.map((r) => [r.key, r]));
		for (const r of rows) byKey.set(r.key, r);
		overrideRows = [...byKey.values()];
		// Backstop for a destination whose schema the client could not read
		// (e.g. an unparseable schema string): a key the server itself reports
		// as needing a value is, by construction, declared there.
		let applied = false;
		const next = { ...overrides };
		for (const r of rows) {
			if (!(r.key in overrides) && r.key in retainedValues) {
				next[r.key] = retainedValues[r.key];
				applied = true;
			}
		}
		if (applied) overrides = next;
		return applied;
	}

	function toFieldDef(row: ItemCopyPreflightNeedsValue): FieldDef {
		return {
			key: row.key,
			label: row.label || row.key,
			type: (row.type ?? 'text') as FieldDef['type'],
			options: row.options,
			required: row.required
		};
	}

	// ── Error classification ──────────────────────────────────────────────

	function errorCode(e: unknown): string | undefined {
		return e instanceof PadApiError ? e.code : undefined;
	}

	/** PRE-WRITE destination failures: the target collection was soft-deleted, or
	 *  access to the destination was revoked. The server's own sentinels are
	 *  explicit that nothing was inserted, so these must never render as
	 *  outcome-unknown — that would send the user hunting for an item that
	 *  provably does not exist. */
	function isDestinationGoneError(e: unknown): boolean {
		const code = errorCode(e);
		return code === 'collection_not_found' || code === 'forbidden' || code === 'permission_denied';
	}

	/**
	 * Structured codes the server raises BEFORE any write. Everything NOT on
	 * this list — including a generic `Error("API error: 502")`, a rejected
	 * fetch, and any structured code we don't recognise — is treated as
	 * outcome-unknown once the mutation has been dispatched. Erring toward
	 * "unknown" is the safe direction: it withholds a retry that could
	 * duplicate, and tells the user to look.
	 */
	const PRE_WRITE_CODES = new Set([
		// The copy handler's own documented refusals (handlers_items_copy.go):
		'invalid_body', // 400 — the body did not decode
		'missing_field', // 400 — target_workspace / target_collection absent
		'malformed_override', // 400 — override names an undeclared field
		'validation_error', // 400 — the FINAL destination fields are invalid
		'not_found', // 404 — source item absent or not visible
		'actor_required', // 403 — nothing to attribute the copy to
		'conflict', // 409 — unique collision in the destination
		'cross_backend_attachments', // 409 — attachments would cross backends
		// Shared with the preflight / the middleware stack:
		'invalid_override',
		'missing_required_fields',
		'archived',
		'plan_limit_exceeded',
		'rate_limited',
		'unauthorized',
		// Rejected by the middleware stack BEFORE handleCopyItem runs, so they
		// provably cannot have created anything (final review round 2):
		'csrf_error', // 403 — CSRF check failed at the perimeter
		'email_not_verified', // 403 — unverified email may not mutate content
		// 500 internal_error is pre-write ON THIS ROUTE specifically: the only
		// writeInternalError call sites reachable here are in
		// resolveAuthorizedCopy (handlers_items_copy_resolve.go:128,184), both
		// before the store call. A post-commit panic deliberately does NOT
		// emit it — afterCopyCommit logs and lets the response stand — and
		// chi's Recoverer returns a bodiless 500, which has no code and so
		// still falls through to outcome-unknown below.
		'internal_error'
	]);

	function planLimitMessage(e: unknown): string {
		const d = (e instanceof PadApiError ? e.details : undefined) ?? {};
		const plan = typeof d.plan === 'string' ? d.plan : '';
		const limit = typeof d.limit === 'number' ? d.limit : undefined;
		const current = typeof d.current === 'number' ? d.current : undefined;
		const detail =
			limit !== undefined && current !== undefined
				? ` It holds ${current} of ${limit} items${plan ? ` on the ${plan} plan` : ''}.`
				: '';
		return `${destWorkspaceName} is at its item limit, so the copy was refused — nothing was created.${detail}`;
	}

	function handleDestinationLost() {
		// Retain `overrides` — the entered values are still valid input for a
		// replacement destination. Invalidate everything that names the old one.
		destinationLost = `That destination is no longer available — the collection may have been archived, or your access to ${destWorkspaceName} changed. Pick another destination; your entered values are kept.`;
		preflight = null;
		preflightError = '';
		submitError = '';
		destColl = '';
		overrideRows = [];
		overrides = {};
		// Refetch + re-filter through canEditCollection: revocation is exactly
		// why this list is refetched rather than cached.
		void loadDestCollections(destWs);
	}

	// ── Submit ────────────────────────────────────────────────────────────

	/**
	 * A fingerprint of what the user actually REVIEWED — the three buckets and
	 * the warning set. Compared against the pre-submit preflight to detect the
	 * source changing under an open dialog.
	 *
	 * ADVISORY ONLY, deliberately. The preflight is an unlocked snapshot and the
	 * mutation copies the latest LOCKED source, so this cannot promise
	 * preview/commit consistency — it only catches a change that happened to
	 * land between the two reads. A server-side revision token was evaluated and
	 * cut (IDEA-2378): `updated_at` is second-precision, so it could not have
	 * delivered the guarantee either. Do not over-trust this check.
	 */
	function reviewFingerprint(p: ItemCopyPreflight): string {
		return JSON.stringify({ f: p.fields, w: p.warnings, a: p.archive_source });
	}

	async function handleConfirm() {
		if (!canConfirm || !preflight) return;
		const gen = flowGen;
		// Capture the preview generation AND the request. The form stays
		// interactive while we flush + re-check (nothing has been dispatched, so
		// dismissal and edits are still legitimate), which means the destination
		// could change underneath us — and re-reading it at dispatch time would
		// submit to a destination the user never reviewed. A change here simply
		// abandons this attempt; the new destination re-previews and Confirm
		// re-enables.
		const pgen = previewGen;
		const ogen = overrideGen;
		const req = buildRequest();
		const superseded = () =>
			gen !== flowGen || pgen !== previewGen || ogen !== overrideGen;
		const reviewed = reviewFingerprint(preflight);
		preparing = true;
		submitError = '';
		staleReview = '';
		try {
			// 1. Flush the live collab document. `items.content` can lag the
			//    Y.Doc by up to 5s, and the copy reads items.content — so a failed
			//    flush BLOCKS rather than silently copying stale content.
			const flushed = await flushContent();
			if (superseded()) return;
			if (!flushed) {
				submitError =
					'Your latest edits could not be saved, so the copy was not started. Try again in a moment.';
				return;
			}

			// 2. Re-run the preflight immediately before dispatch (advisory).
			let fresh: ItemCopyPreflight;
			try {
				fresh = await api.items.copyPreflight(sourceWsSlug, item.slug, req);
			} catch (e: any) {
				if (superseded()) return;
				if (isDestinationGoneError(e)) {
					handleDestinationLost();
					return;
				}
				// Nothing has been dispatched yet, so this is an ordinary,
				// freely-retryable failure.
				submitError = e?.message ?? 'Could not re-check the copy. Nothing was copied.';
				return;
			}
			if (superseded()) return;
			preflight = fresh;
			mergeOverrideRows(fresh.fields.needs_value);
			if (!fresh.valid) {
				submitError =
					'This item changed and the destination now needs more information. Review the fields below.';
				return;
			}
			if (reviewFingerprint(fresh) !== reviewed) {
				staleReview =
					'This item changed since you reviewed it. The preview below has been refreshed — confirm again to proceed.';
				return;
			}

			// 3. Dispatch. From here, dismissal is vetoed and no failure may be
			//    presented as "nothing happened".
			if (crossWorkspace) {
				await dispatchCopy(gen, req);
			} else {
				// Same captured request for both paths, so the two can never
				// disagree about what was submitted.
				await dispatchMove(gen, req.target_collection, req.field_overrides ?? {});
			}
		} finally {
			// Cleared UNCONDITIONALLY. `preparing` is a busy flag for this dialog
			// instance, not per-attempt state, and `canConfirm` already prevents a
			// second overlapping attempt — so there is nothing for a generation
			// guard to protect, while guarding it strands the flag (and Confirm
			// with it) whenever the destination changes mid-flush.
			preparing = false;
		}
	}

	async function dispatchCopy(gen: number, req: ItemCopyPreflightRequest) {
		submitting = true;
		try {
			const result = await api.items.copy(sourceWsSlug, item.slug, req);
			if (gen !== flowGen) return;
			oncopied(result);
			onclose();
		} catch (e: any) {
			if (gen !== flowGen) return;
			const code = errorCode(e);
			if (isDestinationGoneError(e)) {
				handleDestinationLost();
				return;
			}
			if (code === 'plan_limit_exceeded') {
				submitError = planLimitMessage(e);
				return;
			}
			if (code === 'conflict') {
				submitError = `${e?.message ?? 'A conflicting item already exists in the destination.'} Nothing was copied.`;
				return;
			}
			if (code && PRE_WRITE_CODES.has(code)) {
				submitError = `${e?.message ?? 'The copy was refused.'} Nothing was copied.`;
				return;
			}
			// `copy_failed`, an unstructured non-JSON response, a rejected fetch,
			// a timeout, or a structured code we don't recognise — the request was
			// DISPATCHED, so the outcome is unknown. No retry (DR-13).
			outcomeUnknown =
				e?.message ??
				'The connection was lost after the request was sent, so the outcome is unknown.';
		} finally {
			// Unconditional for the same reason as `preparing` above.
			submitting = false;
		}
	}

	async function dispatchMove(
		gen: number,
		targetCollection: string,
		fieldOverrides: Record<string, unknown>,
	) {
		submitting = true;
		try {
			const outcome = await onmove(targetCollection, fieldOverrides);
			if (gen !== flowGen) return;
			if (outcome.status === 'ok') {
				onclose();
				return;
			}
			if (outcome.status === 'cancelled') {
				submitError = 'Move cancelled. Nothing was changed.';
				return;
			}
			submitError = outcome.message ?? 'Could not move the item.';
		} finally {
			submitting = false;
		}
	}

	// ── Formatting ────────────────────────────────────────────────────────

	function formatBytes(n: number): string {
		if (!n) return '0 B';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		let v = n;
		let i = 0;
		while (v >= 1024 && i < units.length - 1) {
			v /= 1024;
			i++;
		}
		return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
	}

	function humanizeLinkType(t: string): string {
		return t.replace(/[_-]+/g, ' ');
	}

	function displayValue(v: unknown): string {
		if (v === null || v === undefined || v === '') return '—';
		if (Array.isArray(v)) return v.join(', ');
		if (typeof v === 'object') return JSON.stringify(v);
		return String(v);
	}

	function dropReason(reason: string): string {
		switch (reason) {
			case 'no_target_field':
				return 'no matching field in the destination';
			case 'incompatible_type':
				return 'the destination field has a different type';
			case 'undeclared_source_field':
				return 'not declared by this item’s own collection';
			case 'assignee_not_a_member':
				return 'the assignee is not a member of the destination';
			case 'agent_role_not_portable':
				return 'agent roles are workspace-local';
			// BUG-2674 added this reason server-side and nothing here learned it,
			// so it rendered through the fallback as the raw enum string. Rare
			// while only `github_pr` produced it; routine since TASK-2878, which
			// emits it for every carried relation value on a cross-workspace copy.
			case 'referent_not_portable':
				return 'it points at something in the source workspace';
			// The three same-workspace referent failures (TASK-2878). Worth
			// separate sentences: "no such item" and "wrong collection" send the
			// reader to different fixes, and a missing target is a schema problem
			// rather than anything about this item.
			// NEUTRAL WORDING, deliberately. `not_found` is what the server
			// collapses a hidden target to as well as a missing one — telling
			// them apart is the existence oracle it exists to prevent — so a
			// sentence asserting non-existence is both wrong for half the
			// cases and a claim the response cannot support.
			case 'not_found':
				return 'the item it refers to could not be found';
			case 'wrong_collection':
				return 'it refers to an item outside the field’s collection';
			// Covers both "no target collection declared" and "the declared
			// collection is not in this workspace"; the first wording named
			// only the former and misdiagnosed the latter.
			case 'target_missing':
				return 'the field has no valid collection to link to';
			case 'invalid_shape':
				return 'the destination field’s default is not a valid reference';
			default:
				return reason;
		}
	}

	/** Focus the first destination control when the dialog opens, so keyboard
	 *  users land on the thing they must choose rather than the heading. */
	let wsSelectEl = $state<HTMLSelectElement | undefined>(undefined);
	$effect(() => {
		if (open && wsSelectEl) {
			void tick().then(() => wsSelectEl?.focus());
		}
	});
</script>

<Modal
	{open}
	onclose={handleDismiss}
	labelledby={titleId}
	maxWidth="620px"
	closeOnBackdrop={!submitting}
	class="copy-dialog"
>
	<div class="modal-header">
		<h2 id={titleId}>Copy or move {sourceRef}</h2>
		<button
			class="close-btn"
			type="button"
			aria-label="Close"
			disabled={submitting}
			onclick={handleDismiss}>&#10005;</button
		>
	</div>

	<div class="modal-body">
		{#if sourceUnavailable}
			<p class="notice notice-error" role="status">
				This item is no longer available — it was archived or deleted. Nothing can be copied
				from it.
			</p>
		{/if}

		<!-- ── Destination ───────────────────────────────────────────────── -->
		<section class="section">
			<h3 class="section-title">Destination</h3>

			{#if workspacesLoading}
				<p class="muted">Loading workspaces…</p>
			{:else if workspacesError}
				<p class="notice notice-error" role="alert">{workspacesError}</p>
			{:else}
				<label class="field-label" for="copy-dest-ws-{uid}">Workspace</label>
				<select
					id="copy-dest-ws-{uid}"
					class="select"
					bind:this={wsSelectEl}
					value={destWs}
					disabled={submitting}
					onchange={(e) => handleWorkspaceChange(e.currentTarget.value)}
				>
					{#each workspaces as ws (ws.slug)}
						<option value={ws.slug}>{ws.name}{ws.slug === sourceWsSlug ? ' (current)' : ''}</option
						>
					{/each}
				</select>
			{/if}

			{#if destinationLost}
				<p class="notice notice-error" role="alert">{destinationLost}</p>
			{/if}

			<label class="field-label" for="copy-dest-coll-{uid}">Collection</label>
			{#if destCollectionsLoading}
				<p class="muted">Loading collections…</p>
			{:else if destCollectionsError}
				<p class="notice notice-error" role="alert">{destCollectionsError}</p>
			{:else if destCollections.length === 0}
				<p class="muted">No editable collections in this workspace.</p>
			{:else}
				<select
					id="copy-dest-coll-{uid}"
					class="select"
					value={destColl}
					disabled={submitting}
					onchange={(e) => handleCollectionChange(e.currentTarget.value)}
				>
					<option value="">Choose a collection…</option>
					{#each destCollections as c (c.slug)}
						<option value={c.slug}>{c.name}</option>
					{/each}
				</select>
			{/if}
		</section>

		<!-- ── Copy vs move ──────────────────────────────────────────────── -->
		<section class="section">
			<h3 class="section-title" id="copy-mode-label-{uid}">Action</h3>
			<div class="mode-toggle" role="radiogroup" aria-labelledby="copy-mode-label-{uid}">
				<label class="mode-option" class:disabled={!crossWorkspace}>
					<input
						type="radio"
						name="copy-mode-{uid}"
						value="copy"
						checked={effectiveMode === 'copy'}
						disabled={!crossWorkspace || submitting}
						onchange={() => handleModeChange('copy')}
					/>
					<span>Copy — leave the original in place</span>
				</label>
				<label class="mode-option">
					<input
						type="radio"
						name="copy-mode-{uid}"
						value="move"
						checked={effectiveMode === 'move'}
						disabled={submitting}
						onchange={() => handleModeChange('move')}
					/>
					<span>
						{crossWorkspace
							? 'Move — copy, then archive the original'
							: 'Move to another collection in this workspace'}
					</span>
				</label>
			</div>
			{#if !crossWorkspace}
				<p class="muted">
					Within this workspace only a move is available — the item keeps its ID, parent,
					relationships and attachments. Choose another workspace to copy.
				</p>
			{/if}
		</section>

		<!-- ── Preview ───────────────────────────────────────────────────── -->
		{#if destColl}
			<section class="section">
				<h3 class="section-title">Preview</h3>
				{#if preflightLoading && !preflight}
					<p class="muted">Checking what carries over…</p>
				{:else if preflightError}
					<p class="notice notice-error" role="alert">{preflightError}</p>
				{:else if preflight}
					<div class="preview" class:stale={preflightLoading}>
						<!-- Carried -->
						<div class="bucket">
							<h4 class="bucket-title">Carried over ({preflight.fields.carried.length})</h4>
							{#if preflight.fields.carried.length === 0}
								<p class="muted">Nothing carries over from this item’s fields.</p>
							{:else}
								<ul class="bucket-list">
									{#each preflight.fields.carried as f (f.key)}
										<li>
											<span class="k">{f.label || f.key}</span>
											<span class="v">{displayValue(f.value)}</span>
											{#if f.from !== 'migrated'}
												<span class="tag">{f.from === 'override' ? 'your value' : 'default'}</span>
											{/if}
										</li>
									{/each}
								</ul>
							{/if}
						</div>

						<!-- Dropped -->
						{#if preflight.fields.dropped.length > 0}
							<div class="bucket">
								<h4 class="bucket-title">
									Not carried over ({preflight.fields.dropped.length})
								</h4>
								<ul class="bucket-list">
									{#each preflight.fields.dropped as f (f.key + f.kind)}
										<li>
											<span class="k">{f.label || f.key}</span>
											<span class="v muted">{dropReason(f.reason)}</span>
										</li>
									{/each}
								</ul>
							</div>
						{/if}

						<!-- Needs a value -->
						{#if overrideRows.length > 0}
							<div class="bucket">
								<h4 class="bucket-title">Needs a value</h4>
								{#if blockedFields.length > 0}
									<p class="notice notice-error" role="alert">
										{#each blockedFields as f (f.key)}
											<span class="blocked-line">
												<strong>{f.label || f.key}</strong> is a required
												<code>{f.type ?? 'unknown'}</code> field. This dialog can’t collect a value
												for that type safely.
											</span>
										{/each}
										Use the CLI instead:
										<code
											>pad item copy {sourceRef} --to-workspace {destWs} --collection {destColl}
											--field {blockedFields[0].key}=value</code
										>
									</p>
								{/if}
								<div class="needs-list">
									{#each overrideRows as row (row.key)}
										{#if COLLECTABLE_TYPES.has(row.type ?? 'text')}
											<div class="needs-row">
												<span class="k">
													{row.label || row.key}
													{#if row.required}<span class="req" aria-hidden="true">*</span>{/if}
												</span>
												<div class="needs-control">
													<FieldEditor
														field={toFieldDef(row)}
														ariaLabel={row.label || row.key}
														value={overrides[row.key]}
														readonly={submitting || preparing}
														onchange={(v) => handleOverrideChange(row.key, v)}
													/>
													{#if row.message}
														<span class="muted">{row.message}</span>
													{/if}
												</div>
											</div>
										{/if}
									{/each}
								</div>
							</div>
						{/if}

						<!-- Warnings. Cross-workspace only: on an intra-workspace move
						     the item keeps its id, parent, links and attachments, so the
						     preflight's copy-shaped warnings would state the opposite of
						     what happens. -->
						{#if crossWorkspace && warnings}
							<div class="bucket">
								<h4 class="bucket-title">What you should know</h4>
								<ul class="warn-list">
									{#if warnings.child_count > 0}
										<li>
											{warnings.child_count} child item{warnings.child_count === 1 ? '' : 's'}
											{warnings.children_orphaned
												? ' will be left behind in this workspace with their parent archived.'
												: ' will NOT be copied and stay here.'}
										</li>
									{/if}
									{#if warnings.dropped_parent}
										<li>The copy will have no parent — the parent link is not carried.</li>
									{/if}
									{#each linkRows as r (r.dir + r.type)}
										<li>
											{r.count}
											{humanizeLinkType(r.type)}
											{r.dir === 'outgoing' ? 'relationship' : 'inbound relationship'}{r.count === 1
												? ''
												: 's'} will not be carried.
										</li>
									{/each}
									{#if warnings.dropped_assignee}
										<li>The assignee is not a member of the destination, so it is cleared.</li>
									{/if}
									{#if warnings.dropped_agent_role}
										<li>The agent role is workspace-local and is cleared.</li>
									{/if}
									{#if warnings.attachment_count > 0}
										<li>
											{warnings.attachment_count} attachment{warnings.attachment_count === 1
												? ''
												: 's'} ({formatBytes(warnings.attachment_bytes)}, including thumbnails)
											will be copied — this ADDS that much to {preflight.destination.workspace_name}.
										</li>
									{/if}
									{#if warnings.unresolvable_ref_count > 0}
										<li>
											{warnings.unresolvable_ref_count} attachment reference{warnings.unresolvable_ref_count ===
											1
												? ''
												: 's'} in the content already resolve to nothing and stay broken.
										</li>
									{/if}
									{#if !warnings.child_count && !warnings.dropped_parent && linkRows.length === 0 && !warnings.dropped_assignee && !warnings.dropped_agent_role && !warnings.attachment_count && !warnings.unresolvable_ref_count}
										<li>Nothing else is affected.</li>
									{/if}
								</ul>
								{#if warnings.relationships_partial}
									<!-- Floor qualifier, NOT a warning row. Some of this
									     item's relatives are on items this account cannot
									     see, and the server does not count what it will not
									     show. Rendered only when true — a marker that fires
									     on the common case becomes noise. `children_orphaned`
									     is qualified only on the move path: a plain copy
									     archives nothing, so no hidden child can be orphaned
									     by it and `false` is the complete answer there. -->
									<p class="notice notice-info">
										These relationship counts are a <strong>floor, not a total</strong>: at least
										one of this item’s relationships is on an item you can’t see, and isn’t
										counted{archiveSource ? ', including any children that would be left behind' : ''}.
									</p>
								{/if}
							</div>
						{/if}

						<!-- Permanent content-semantics note (DR-21). Not conditional:
						     the counts alone never tell the user what happens to the
						     body of their item, and wiki-link RETARGETING is the
						     surprising outcome. -->
						{#if crossWorkspace}
							<p class="notice notice-info content-note">
								The content is copied as written. Attachment references are repointed at the
								copied files — but only those that still resolve to a live attachment in this
								workspace; any others are left as they are and counted above. And
								<code>[[wiki-links]]</code> are not rewritten: they are re-resolved in
								{preflight.destination.workspace_name}, so a link can end up pointing at a
								<strong>different item</strong> there, or at nothing. Explicit
								<code>[[workspace::REF]]</code> links keep pointing back here.
							</p>
						{/if}
					</div>
				{/if}
			</section>
		{/if}

		<!-- ── Outcome states ────────────────────────────────────────────── -->
		{#if outcomeUnknown}
			<p class="notice notice-error" role="alert">
				<strong>The outcome of this {effectiveMode} is unknown.</strong>
				{outcomeUnknown} The request had already been sent, so it may have completed. Open
				<strong>{destWorkspaceName}</strong> and check before doing anything else — do
				<strong>not</strong> retry, or you may create a duplicate.
			</p>
		{:else if staleReview}
			<p class="notice notice-warn" role="status">{staleReview}</p>
		{:else if submitError}
			<p class="notice notice-error" role="alert">{submitError}</p>
		{/if}
	</div>

	<div class="modal-footer">
		{#if submitting}
			<span class="muted footer-status" role="status">
				{effectiveMode === 'move' ? 'Moving' : 'Copying'} — don’t close this window.
			</span>
		{/if}
		<button class="btn" type="button" disabled={submitting} onclick={handleDismiss}>
			{outcomeUnknown ? 'Close' : 'Cancel'}
		</button>
		<button class="btn btn-primary" type="button" disabled={!canConfirm} onclick={handleConfirm}>
			{confirmLabel}
		</button>
	</div>
</Modal>

<style>
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-4);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1rem;
		font-weight: 600;
	}
	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 1rem;
		line-height: 1;
		padding: var(--space-1);
	}
	.close-btn:disabled {
		opacity: 0.4;
		cursor: not-allowed;
	}

	/* The body is the ONLY scroll container: Modal caps the dialog at 85vh and
	   hides overflow, and this dialog can carry the full warning set plus one
	   typed input per unsatisfied required field. Keeping the scroll here means
	   the footer controls stay reachable at every viewport. */
	.modal-body {
		padding: var(--space-4);
		overflow-y: auto;
		flex: 1 1 auto;
		min-height: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.modal-footer {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-3) var(--space-4);
		border-top: 1px solid var(--border);
		flex-shrink: 0;
	}
	.footer-status {
		margin-right: auto;
	}

	.section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.section-title {
		margin: 0;
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
	}
	.field-label {
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.select {
		width: 100%;
		padding: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 0.9rem;
	}

	.mode-toggle {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.mode-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.9rem;
	}
	.mode-option.disabled {
		opacity: 0.55;
	}

	.preview {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.preview.stale {
		opacity: 0.6;
	}
	.bucket {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.bucket-title {
		margin: 0;
		font-size: 0.85rem;
		font-weight: 600;
	}
	.bucket-list,
	.warn-list {
		margin: 0;
		padding-left: var(--space-4);
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: 0.85rem;
	}
	.bucket-list {
		list-style: none;
		padding-left: 0;
	}
	.bucket-list li {
		display: flex;
		gap: var(--space-2);
		align-items: baseline;
		flex-wrap: wrap;
	}
	.k {
		font-weight: 500;
		min-width: 8rem;
	}
	.v {
		color: var(--text-secondary);
		overflow-wrap: anywhere;
	}
	.tag {
		font-size: 0.7rem;
		padding: 0 var(--space-1);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
	}
	.req {
		color: var(--accent-orange);
	}

	.needs-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.needs-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		flex-wrap: wrap;
	}
	.needs-control {
		flex: 1 1 12rem;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.notice {
		margin: 0;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.85rem;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
	}
	.notice-error {
		border-left: 3px solid var(--accent-red, var(--accent-orange));
	}
	.notice-warn {
		border-left: 3px solid var(--accent-orange);
	}
	.notice-info {
		border-left: 3px solid var(--border);
		color: var(--text-secondary);
	}
	.content-note code {
		font-size: 0.8em;
	}
	.blocked-line {
		display: block;
	}

	.muted {
		margin: 0;
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.btn {
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: var(--text-primary);
		font-size: 0.85rem;
		cursor: pointer;
	}
	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.btn-primary {
		background: var(--accent);
		border-color: var(--accent);
		color: #fff;
	}
</style>
