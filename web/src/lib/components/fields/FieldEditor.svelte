<!--
@component
Custom-styled field editor for item detail pages.
Supports text, number, select, multi_select, date, checkbox, and url field types.

Usage:
```svelte
<FieldEditor {field} {value} onchange={(v) => handleChange(v)} />
```

Pass `readonly={true}` to render a display-only view — used by item detail
pages when the current user lacks edit permission on the item
(PLAN-1100 / TASK-1105). Display mode renders the value with the same
visual language as the editor but with no inputs, dropdowns, or mutation
handlers — onchange is never called.
-->
<script lang="ts">
	import { onDestroy, tick, untrack } from 'svelte';
	import { formatItemRef, type FieldDef, type ItemIndexRow, type PaneTarget } from '$lib/types';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { rawText, readAs } from '$lib/fields/fieldShape';
	import { narrowRelationRow, UNRESOLVED_LABEL, UNRESOLVED_TITLE } from '$lib/collections/relationGroups';
	import {
		isMultiRelationType,
		isRelationType,
		isRelationValueStoredAsText,
		relationValuesOf,
	} from '$lib/items/relationFieldTypes';
	import { WriteOrder } from '$lib/items/fieldWriteOrder';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { titleLimitError } from '$lib/items/titleLimit';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import ItemPicker from '$lib/components/items/ItemPicker.svelte';
	import { shouldOpenInPane } from '$lib/components/collections/itemCardClick';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { clickOutside } from '$lib/utils/clickOutside';
	import { canonicalValueColor, formatFieldLabel as formatLabel } from '$lib/utils/fieldColors';

	interface Props {
		field: FieldDef;
		value: any;
		/**
		 * Write the new value.
		 *
		 * A consumer MAY return something awaitable, and the multi_relation hold
		 * below treats that as the write's settlement signal — success or
		 * failure alike, since either way the write is no longer outstanding.
		 * Returning nothing is still supported and leaves the hold released by
		 * prop agreement alone; that is weaker (a REFUSED write never agrees, so
		 * the hold outlives it), not wrong, and it is what the consumers with no
		 * server behind them do.
		 */
		onchange: (value: any) => void | Promise<void>;
		readonly?: boolean;
		/**
		 * Accessible name for the rendered control. Optional: on the item
		 * detail page the visible field label sits beside the control in a
		 * `.field-row`, so the default (undefined → no `aria-label`) leaves
		 * that markup exactly as it was. Consumers that render the control
		 * WITHOUT an associated <label> — e.g. the cross-workspace copy
		 * dialog's needs-a-value rows — pass the field name here so the
		 * input/trigger is not announced as an unnamed edit field or button.
		 */
		ariaLabel?: string;
		/**
		 * Link context for `relation` fields. All three are optional because the
		 * OTHER call site — `CopyItemDialog`'s needs-a-value rows — has no item
		 * page to link into and, more importantly, no target collection to scope
		 * a picker to: its `FieldDef` is built from a preflight row whose shape
		 * carries no `collection` (TASK-2869 / U2b fixes that end).
		 *
		 * So the relation branch is GATED on `wsSlug` AND `field.collection`, and
		 * renders read-only without them. That is deliberate: an unscoped picker
		 * in the cross-workspace copy dialog would offer SOURCE-workspace items
		 * as the value for a DESTINATION-workspace field and look authoritative
		 * doing it. A field the user cannot fill is the honest state until U2b.
		 */
		wsSlug?: string;
		username?: string;
		onOpenTarget?: (target: PaneTarget) => void;
		/**
		 * The row this editor is pointed at, when the mounting view has one.
		 *
		 * It fences the typed path's echo check (BUG-3039), which asks "did
		 * `value` change because OUR write came home?" — a question the value
		 * alone cannot answer, since another row's value can equal what we just
		 * sent for this one.
		 *
		 * NOT LOAD-BEARING TODAY, and the comment that said otherwise was wrong.
		 * ItemDetail mounts the fields panel inside `{#key itemSlug}`
		 * (PLAN-2105 / TASK-2112), so an item switch DESTROYS this component
		 * rather than retargeting it, and the pending edit dies with it. Kept
		 * anyway, because this component cannot see its caller's structure: a
		 * `{#key}` in another file is exactly the kind of cross-component premise
		 * that went stale under BUG-3039's feet, and a caller that mounts this
		 * without one would silently reopen the question. A caller that omits the
		 * id still gets the echo check — with nothing to retarget between, "same
		 * subject?" answers true rather than answering nothing.
		 *
		 * Deliberately not folded into `relationIdentity`, which fences a
		 * different thing (the workspace and target collection a resolved chip
		 * belongs to) and is asked on a path with no debounce.
		 */
		itemId?: string;
	}

	let { field, value: storedValue, onchange, readonly = false, ariaLabel, wsSlug, username = '', onOpenTarget, itemId }: Props = $props();

	// ── Shape mismatch (BUG-3052 unit 2) ───────────────────────────────────
	//
	// A declared type is not a promise about the stored value: a retype
	// rewrites nothing. A value whose SHAPE does not match (`"5"` under a
	// number, `"false"` under a checkbox) is shown as its raw stored text with a
	// note, never fed to the type's editor: the checkbox read `!!"false"` as
	// checked and its toggle wrote a boolean over the string, and the number
	// step converted the string. The ruling: an edit REPLACES the value
	// explicitly, never a silent coercion. Replace opens the ordinary editor on
	// an EMPTY value; nothing is written until the user sets one.
	//
	// The replace state is KEYED on (item, field, stored value) rather than a
	// flag an effect resets, so switching items or a remote write ends it and
	// the next item cannot inherit it.
	let shape = $derived(readAs(storedValue, field.type));
	let mismatchKey = $derived(`${itemId ?? ''}\u0000${field.key}\u0000${rawText(storedValue)}`);
	let replacingKey = $state<string | null>(null);
	let replacing = $derived(!shape.ok && replacingKey === mismatchKey);
	/** What every editor branch below reads: the stored value, or nothing while replacing it. */
	let value = $derived(shape.ok ? storedValue : undefined);

	// ── Relation resolution ───────────────────────────────────────────────
	//
	// THREE states, not two, and the third is the common one on existing data:
	// `internal/items/validate.go` accepts ANY string for a relation, and this
	// component's own text fallback has been writing arbitrary strings into
	// these fields, so a value that resolves to nothing is what most legacy
	// relation values ARE. It has to be distinguishable from a value whose
	// target was deleted — those are different facts about the item.
	//
	// All three resolve locally: `localIndex` is a workspace-wide read model
	// that holds soft-deleted rows alongside live ones (`getByCollection`
	// filters them out by default rather than dropping them), so a dangling
	// target is a row carrying `deleted_at`. No fetch, and no loading state.
	let isRelation = $derived(isRelationType(field.type));

	/**
	 * The VALUE-level question, and it is a different one from `isRelation`
	 * (see `relationFieldTypes`): a `multi_relation` holds an ORDERED LIST of
	 * references where a `relation` holds one. Everything below that reads or
	 * writes a value has to ask it; everything that asks "does this field name
	 * other items at all" asks `isRelation`.
	 */
	let isMultiRelation = $derived(isMultiRelationType(field.type));

	/**
	 * The field's references as raw strings, in order.
	 *
	 * ONE element for a scalar `relation`, N for a `multi_relation`, none when
	 * the field is empty — so every render path below is driven by a list and
	 * the scalar case is the one-element case rather than a second code path.
	 * That is the whole shape of U4's web half: the resolution, chip and link
	 * logic is parameterised by ONE raw reference, and the two types differ
	 * only in how many of those there are and what a new choice does to them.
	 *
	 * Blank and non-string elements are dropped rather than rendered. The write
	 * doors refuse both outright (`internal/items/validate.go`, the
	 * multi_relation arm — an empty element is an error, not a skip, precisely
	 * so an ordered list's length cannot depend on which elements were blank),
	 * so this is defence against a value no door will accept, not a policy of
	 * its own.
	 */
	let relationValues = $derived.by((): string[] => {
		if (!isRelation) return [];
		if (isMultiRelation) {
			// THE LAST LIST WE SENT WINS OVER THE PROP WHILE A WRITE IS IN
			// FLIGHT (codex round 7). `value` only catches up after the server
			// round trip, so two quick edits both derived from it: remove A then
			// B from [A,B,C] sent [B,C] and then [A,C], and the second write
			// puts A back. Every edit below is a WHOLE-LIST write computed from
			// the current list, which makes a stale base a lost update rather
			// than a harmless recompute — the scalar path has no equivalent
			// exposure because its writes are replacements, not edits.
			//
			// Cleared when the write is OVER — whichever of two signals arrives:
			// the prop agreeing with what we sent, or (for a consumer that
			// returns something awaitable) that write settling, success or
			// failure. Agreement alone was not enough, because a REFUSED write
			// never agrees and the hold outlived it forever (round 8, R8-2). So a
			// change arriving from anywhere else — SSE, another tab, the parent's
			// 409 refetch-and-retry — takes over the moment it lands. This holds
			// a value forward; it does not own it.
			if (pendingRelation && pendingRelation.identity === relationIdentity) return pendingRelation.list;
		}
		// The SHAPE half moved to `relationValuesOf` (BUG-3016), which the table
		// cell needs verbatim: a second copy of this normalisation shows up as a
		// table cell and a properties chip disagreeing about one stored value.
		// The HOLD above stays here — it is about a write this component has in
		// flight, which no read-only surface has.
		return relationValuesOf(field.type, value);
	});

	/**
	 * What this component is resolving against: the workspace, the ITEM, the
	 * field and its type, and the field's declared target. A held list belongs to
	 * ONE of these and to no other. The item and the type joined in TASK-3048:
	 * two items share workspace + key + target, and a field retyped in place
	 * (a live schema edit) keeps its key, so without them a reused editor could
	 * not tell a hold taken for another row, or under another type's value
	 * rules, from its own.
	 */
	let relationIdentity = $derived(
		`${wsSlug ?? ''}\u0000${itemId ?? ''}\u0000${field.key}\u0000${field.type}\u0000${field.collection ?? ''}`
	);

	/**
	 * The list this component last SENT, held until that write is OVER, STAMPED
	 * with the identity it was sent for.
	 *
	 * "Over" is two signals, not one: `value` reflecting what we sent, or the
	 * consumer's own settlement when it gives one. The second was added in round
	 * 8 (R8-2) because the first cannot answer for a write the server REFUSES —
	 * that value never comes back, so the hold kept a rejected list on screen
	 * and ignored every later server value.
	 *
	 * The stamp is the fence, and it is structural on purpose. My first version
	 * cleared the hold from an `$effect` that read `wsSlug` and `field.key`
	 * directly — which re-runs on every prop assignment, equal or not, so an
	 * ordinary parent re-render mid-write released the hold and the stale prop
	 * came back as the base. The fix worked only in a test whose two clicks had
	 * nothing in between. Comparing a stamp asks the question the fence is for
	 * ("is this still the same item?") instead of a question that happens to
	 * correlate with it ("did anything re-render?").
	 *
	 * `$state` rather than a plain `let` (CONVE-1688's split): `relationValues`
	 * reads it, so it belongs in the effect graph.
	 */
	let pendingRelation = $state<{
		identity: string;
		list: string[];
		/**
		 * True when the consumer gave a settlement signal, which then OWNS the
		 * release and prop agreement must keep out of it.
		 *
		 * Agreement is a coincidence test, not an answer: remove C from
		 * [A,B,C] and add it straight back, and the second write is holding
		 * [A,B,C] — equal to the prop nobody has changed yet. The effect below
		 * read that as "the server confirmed us", released mid-write, and the
		 * FIRST write's response then arrived as [A,B] and became the base. C
		 * was lost by the mechanism built to stop exactly that.
		 */
		tracked: boolean;
	} | null>(null);

	/**
	 * Tickets for the hold, so the SETTLEMENT of an older write cannot release a
	 * newer one's hold. Same class the item pane orders its field writes with
	 * (`$lib/items/fieldWriteOrder`) — one model, asked in two places, because
	 * the hold and the pane's 409 retry are two halves of one question about
	 * which whole-list write is current (codex round 8, R8-1 / R8-2).
	 *
	 * Keyed by `relationIdentity`, so a retarget starts its own sequence.
	 */
	const holdOrder = new WriteOrder();

	/**
	 * The consumer's return value as a settlement signal, or null when it gave
	 * none. Duck-typed on `then` rather than `instanceof Promise` so an async
	 * handler wrapped by a test double or a framework thenable still counts.
	 */
	function settlementOf(outcome: unknown): Promise<unknown> | null {
		const thenable = outcome as { then?: unknown } | null | undefined;
		return typeof thenable?.then === 'function' ? (outcome as Promise<unknown>) : null;
	}

	/**
	 * Send a whole-list write and remember it as the base for the next edit.
	 *
	 * ONE function, because every list mutation is "compute the new list, send
	 * it" and a second copy is how one of them ends up basing itself on the
	 * prop again.
	 */
	function writeRelationList(next: string[]) {
		const identity = relationIdentity;
		const ticket = holdOrder.take(identity);
		let outcome: void | Promise<void>;
		try {
			pendingRelation = { identity, list: next, tracked: false };
			outcome = onchange(next);
		} catch (err) {
			// A consumer that throws SYNCHRONOUSLY never reaches the settlement
			// path below, so the hold it just armed would stand forever and
			// every later edit would base on the abandoned list. Release and
			// rethrow: the throw is the consumer's to report, the stranded hold
			// was ours.
			if (pendingRelation?.identity === identity && !holdOrder.superseded(ticket)) {
				pendingRelation = null;
			}
			throw err;
		}
		// A consumer that returns nothing has told us nothing, and the hold falls
		// back to prop agreement (below). One that returns a promise is telling
		// us when its write SETTLED, which is the question the hold is actually
		// asking — agreement answers it only for writes that succeed, so a
		// REFUSED write (a required field cleared, a validation error) left the
		// rejected list on screen forever and ignored every later server value.
		const settlement = settlementOf(outcome);
		if (!settlement) return;
		// Settlement owns the release from here — see `tracked`.
		pendingRelation = { identity, list: next, tracked: true };
		void (async () => {
			// The release is in a FINALLY because it must run under ANY identity
			// (BUG-3105): the hold shows the list this identity chose, so a
			// release refused across a sign-out would leave that list on screen
			// for the next user. It assigns a literal, which carries no data across
			// the await — the finally-literal rule the fence guard applies.
			try {
				try {
					await settlement;
				} catch {
					// Settled is settled. The consumer owns error reporting — the
					// only thing that changes here is that we stop holding, which is
					// as true of a failure as of a success.
				}
				// Flush before releasing, for a consumer that RESOLVES BEFORE IT
				// ASSIGNS. `ItemDetail` assigns first, so nothing can currently
				// observe this line and no test kills it — stated rather than
				// dressed up as tested, because a mutant removing it survives the
				// whole file. It is kept, unlike W7's duplicate guard, because that
				// guard was unreachable BY CONSTRUCTION while this one is reachable
				// by a consumer shape the signature permits: `Promise<void>` says
				// when the write ended, never when the value landed. Releasing ahead
				// of the assignment would show the pre-write list and hand an edit
				// started in that window the stale base — the round-7 defect again.
				await tick();
			} finally {
				if (!holdOrder.superseded(ticket) && pendingRelation?.identity === identity) {
					pendingRelation = null;
				}
			}
		})();
	}

	// Release the hold as soon as the prop agrees — the FIRST of the two release
	// signals, and the only one for a consumer that answers nothing. Comparing
	// CONTENT, not array identity: the value comes back through JSON, so it is
	// never the same array we sent. A prop that disagrees is still stale — keep
	// holding, unless the write itself has settled (`writeRelationList`). A change
	// arriving from ELSEWHERE while we hold is the case this cannot distinguish,
	// and it resolves itself: the next agreement releases, and until then the
	// user is editing the list they last acted on, which is the one on screen.
	$effect(() => {
		const pending = pendingRelation;
		if (!pending) return;
		// Not for a write that reports its own settlement, and not for a hold
		// belonging to a DIFFERENT identity: a retargeted editor whose new value
		// happens to equal the old one's held list would otherwise clear a hold
		// that was never about this field.
		if (pending.tracked || pending.identity !== relationIdentity) return;
		const incoming = Array.isArray(value)
			? value.map((e) => (typeof e === 'string' ? e.trim() : '')).filter((e) => e !== '')
			: [];
		if (incoming.length === pending.list.length && incoming.every((e, i) => e === pending.list[i])) {
			pendingRelation = null;
		}
	});

	// Discard a hold when the editor is RETARGETED, so X→Y→X does not revive a
	// hold from before the excursion (TASK-3048). The stamp fence above already
	// stops a foreign hold being shown or released by agreement; what it left is
	// a DORMANT hold that comes back to life on the return, over a value that
	// may have moved meanwhile. The write it protected still settles through its
	// ticket (`holdOrder`), and its release finds nothing to release.
	//
	// The trigger is a real CHANGE of the identity string, compared against the
	// previous one, never "the effect re-ran": the note on `pendingRelation`
	// records that clearing from an effect reading the props directly released
	// holds on ordinary re-renders mid-write. `lastRelationIdentity` is a plain `let` so writing
	// it does not re-trigger this effect, and the hold is read untracked so a
	// new hold being armed does not either.
	let lastRelationIdentity: string | undefined;
	$effect(() => {
		const identity = relationIdentity;
		if (lastRelationIdentity !== undefined && identity !== lastRelationIdentity) {
			const pending = untrack(() => pendingRelation);
			if (pending && pending.identity !== identity) pendingRelation = null;
		}
		lastRelationIdentity = identity;
	});

	/**
	 * Whether the field's declared target still names a live collection.
	 *
	 * `FieldDef.collection` holds a SLUG (the schema editor binds
	 * `<option value={c.slug}>`), and renaming a collection changes its slug
	 * WITHOUT migrating the relation definitions that point at it — nothing in
	 * `store.UpdateCollection` touches them. So a rename silently strands every
	 * relation field aimed at that collection. Filed separately; this component
	 * only has to behave sanely in the meantime.
	 *
	 * Three-valued on purpose. `'unknown'` is when the collection list has not
	 * loaded for this workspace yet — the absence of evidence, which must not be
	 * read as a stale target, since that would flash every relation field into a
	 * broken state on first paint.
	 */
	let knownCollectionSlugs = $derived.by((): Set<string> | null => {
		if (!wsSlug || !collectionStore.collectionsAreFreshFor(wsSlug)) return null;
		return new Set((collectionStore.collections ?? []).map((c) => c.slug));
	});

	let relationTarget = $derived.by((): 'live' | 'stale' | 'unknown' => {
		if (!isRelation || !wsSlug || !field.collection) return 'unknown';
		if (!knownCollectionSlugs) return 'unknown';
		return knownCollectionSlugs.has(field.collection) ? 'live' : 'stale';
	});

	// A picker aimed at a renamed collection would list NOTHING — `getByCollection`
	// and `localSearch` both filter on that slug. Read-only with the value still
	// legible beats a search box that silently never matches.
	let relationEditable = $derived(
		isRelation && !!wsSlug && !!field.collection && relationTarget !== 'stale',
	);
	//
	// FUNCTIONS OF ONE RAW REFERENCE, not of `value` (U4/W7). These four used
	// to be `$derived` values closing over the scalar `value`; a
	// `multi_relation` needs every one of them answered per ELEMENT, and a
	// second copy taking a parameter is how the two types drift apart. So the
	// parameterised form is the only implementation and the scalar path below
	// calls it with its single element — which is also why the existing scalar
	// render tests are the instrument that this refactor moved nothing.
	function relationRowFor(raw: string): ItemIndexRow | null {
		if (!isRelation || !wsSlug) return null;
		const ref = raw.trim();
		if (!ref) return null;
		// ONE IMPLEMENTATION OF THE TWO NARROWINGS (TASK-2998). They were worked
		// out here and duplicated into `relationGroups` when the board needed
		// them; TASK-2996 has merged without touching this file, so the fork is
		// closed rather than left as a note. The reasoning — why an id-only
		// match, why the collection check only fires when both slugs are known —
		// lives on `narrowRelationRow`.
		//
		// `relationTarget === 'live'` and "the collection list knows the
		// declared slug" are the same condition: `relationTarget` is derived
		// from that same list, so the helper's own guard covers the stale and
		// unknown cases this branch used to name.
		return narrowRelationRow(
			localIndex.findByIdOrSlug(wsSlug, ref),
			ref,
			field.collection,
			knownCollectionSlugs,
		);
	}
	function relationStateFor(raw: string): 'empty' | 'live' | 'deleted' | 'unresolved' {
		if (!isRelation) return 'empty';
		if (!raw.trim()) return 'empty';
		const row = relationRowFor(raw);
		if (!row) return 'unresolved';
		return row.deleted_at ? 'deleted' : 'live';
	}
	function relationRefFor(row: ItemIndexRow | null): string | null {
		return row ? formatItemRef(row) : null;
	}
	function relationHrefFor(row: ItemIndexRow | null): string | null {
		if (!row || !wsSlug || !username) return null;
		const seg = relationRefFor(row) ?? row.slug;
		if (!row.collection_slug || !seg) return null;
		return `/${username}/${wsSlug}/${row.collection_slug}/${seg}`;
	}

	function handleRelationClick(e: MouseEvent, row: ItemIndexRow) {
		if (!shouldOpenInPane(e, !!onOpenTarget)) return;
		e.preventDefault();
		onOpenTarget?.({
			ref: relationRefFor(row) ?? undefined,
			slug: row.slug,
			href: relationHrefFor(row) ?? undefined,
			collectionSlug: row.collection_slug,
		});
	}

	// A relation field shows its VALUE, not a permanently-open search box. The
	// first browser pass rendered the chip, the picker input still holding the
	// query, and the result list still listing the row just chosen — the same
	// item three times, under every relation field on the page. The picker is
	// for CHANGING the value, so it appears when there is nothing to show or
	// when the user asks for it, and closes once a choice is made.
	let editingRelation = $state(false);

	function pickRelation(row: ItemIndexRow) {
		relationWrite++;
		editingRelation = false;
		commitPicked(row.id);
	}

	/**
	 * Write one chosen id into the field, in whichever shape the field takes.
	 *
	 * REPLACE for a `relation`, APPEND for a `multi_relation` — that is the
	 * entire difference between the types at the write end, and it lives in one
	 * function because BOTH ways of choosing (the picker and the inline create)
	 * land here. Two copies of "what choosing means" is how the create path
	 * ends up replacing a list the picker path appends to.
	 */
	function commitPicked(id: string) {
		if (!isMultiRelation) {
			onchange(id);
			return;
		}
		writeRelationList([...relationValues, id]);
	}

	/**
	 * Drop one element, by POSITION rather than by value — the list is ordered
	 * and the user pointed at a row, not at an id.
	 *
	 * The last removal writes `[]`, not an absent key: an empty array is a
	 * valid shape meaning "no targets", and normalising it to absence is the
	 * WRITE DOOR's job (`internal/items/validate.go`, the multi_relation arm,
	 * which spells out why the shape check cannot be the one to decide it).
	 * Guessing at absence here would be this component answering a question the
	 * server already answers, in a second place.
	 */
	function removeRelationAt(index: number) {
		relationWrite++;
		writeRelationList(relationValues.filter((_, i) => i !== index));
	}

	// Backing out is a decision, so it supersedes an in-flight create exactly as
	// picking another row does (codex round 2).
	function cancelRelationEdit() {
		relationWrite++;
		editingRelation = false;
	}

	function clearRelation() {
		relationWrite++;
		editingRelation = false;
		onchange('');
	}

	/**
	 * Fences for the in-flight create (see `createRelationTarget`). Plain `let`
	 * per CONVE-1688 — written and read only in handlers and lifecycle, never
	 * rendered, so neither belongs in the effect graph.
	 */
	let relationWrite = 0;
	let destroyed = false;
	onDestroy(() => {
		destroyed = true;
	});

	// ── Inline create from the picker (PLAN-2857 U8) ─────────────────────
	//
	// The picker offers a create row only when handed an `oncreate`, so this
	// component decides BOTH of U8's gates by deciding whether to pass one.
	//
	// The permission gate is `canEditCollection` on the TARGET collection — the
	// same predicate behind the collection page's "+ New", asked about where the
	// item would LAND rather than about where the user is standing. It needs the
	// collection's ID, which only the loaded collection list has; a target the
	// list does not know yields `null` here and therefore no create row, because
	// "no answer" must not read as "allowed".
	// FRESHNESS, for the same reason `knownCollectionSlugs` has it (codex round
	// 1 P2): `collectionStore.collections` is one global list, so during a
	// workspace switch it still holds the PREVIOUS workspace's rows. A slug
	// match against those yields another workspace's collection ID, and asking
	// `canEditCollection` about that ID is asking the wrong question — it can
	// answer yes and put a create row on a field whose target is not that
	// collection at all.
	let targetCollection = $derived.by(() => {
		if (!isRelation || !field.collection || !wsSlug) return null;
		if (!collectionStore.collectionsAreFreshFor(wsSlug)) return null;
		return (collectionStore.collections ?? []).find((c) => c.slug === field.collection) ?? null;
	});
	let canCreateInTarget = $derived(
		!!targetCollection && workspaceStore.canEditCollection(targetCollection.id),
	);

	/**
	 * Create the item the user just described, then select it.
	 *
	 * NO field values are sent. The server fills every missing key that declares
	 * a `Default` and stores the defaulted map (`items.ValidateFields`, then
	 * "Marshal validated/defaulted fields back" in `createItemChecked`), so the
	 * schema's own answer is already the right one. Guessing here — the
	 * collection page's "+ New" uses `status.options[0]` — would override it,
	 * and would be wrong for any schema whose default is not its first option.
	 * The cost is that a target with a REQUIRED field carrying no default
	 * refuses the create; that surfaces as a toast naming the field, which is
	 * the honest outcome for a row this picker cannot fill in.
	 *
	 * The epoch is read BEFORE the request (BUG-2098): a projection resync while
	 * it is in flight means the response was authorized under a scope that no
	 * longer applies, and `upsert` refuses it rather than resurrecting a row no
	 * delta will evict. Upserting at all is what makes the picker's
	 * exact-title suppression true on the NEXT keystroke — without it the same
	 * text would offer to create a second item.
	 */
	async function createRelationTarget(title: string, pickerIdentity?: () => boolean) {
		const ws = wsSlug;
		const collSlug = field.collection;
		if (!ws || !collSlug) return;
		// BUG-3115: refuse a too-long title before sending; the picker keeps
		// the query.
		const limitError = titleLimitError(title);
		if (limitError) {
			toastStore.show(limitError, 'error');
			return;
		}
		// Two fences on the completion, both found by codex round 1 P1, both
		// about the same gap: the create is a round trip and the picker stays
		// open across it, so the world can move before it lands.
		//
		//   * DESTROYED. `ItemDetail` wraps its fields section in
		//     `{#key itemSlug}`, so an item switch destroys this component — but
		//     not this promise, and `onchange` calls into the PERSISTENT parent,
		//     whose `updateField` builds its PATCH against whatever item is
		//     current at CALL time. Unfenced, a create started on car A writes
		//     its colour onto car B.
		//   * SUPERSEDED. The user can settle on another row, clear the field, or
		//     back out of the picker entirely while this is in flight.
		//     Last-write-wins is the wrong rule: each of those is an explicit
		//     choice and this one is a promise they have moved past.
		//   * RETARGETED. The `{#key itemSlug}` above keys on the SLUG ONLY, so
		//     switching workspaces to an item carrying the same ref — every
		//     workspace has a TASK-5 — reuses this instance and never sets
		//     `destroyed`. Comparing the captured workspace and target collection
		//     to the current props is what catches that (codex round 2).
		const mySeq = ++relationWrite;
		const epoch = localIndex.scopeEpochFor(ws);
		const resetGen = localIndex.resetGenerationFor(ws);

		// Is the index still the one this request was authorized against?
		//
		// Both halves are needed and neither substitutes for the other.
		// `scopeEpoch` moves on a projection RESYNC; `resetGeneration` moves on
		// a DROP. Epoch alone cannot see a drop, because `reset()` deletes the
		// state and the replacement starts at 0 — and 0 is also the value in the
		// overwhelmingly common case where no resync ever happened, so an
		// equality check on it passes trivially across exactly the event it was
		// meant to catch (codex round 6, correcting the residual round 5
		// dismissed as needing a coincidence; it needs none).
		const indexStillOurs = () => localIndex.resetGenerationFor(ws) === resetGen;
		// THREE things are deliberately NOT fenced here, each raised by review
		// and each declined for a reason that belongs next to the code rather
		// than in a commit message nobody downstream reads.
		//
		// 1. A CONCURRENT CHANGE TO THE FIELD (SSE, another tab) landing while
		//    the POST is pending. Overwriting it is ordinary last-write-wins on
		//    a field the user is actively editing, and it is what every other
		//    type in this component already does — a text field blurred after a
		//    remote change overwrites it too. The server, not this component, is
		//    where that race is adjudicated: `ItemDetail.updateField` sends
		//    `expected_updated_at` and refetch-retries a 409 (BUG-2273 /
		//    IDEA-1480). Fencing it HERE would make relation fields alone behave
		//    differently from every other field, on a rule the item's own
		//    optimistic-concurrency check already enforces.
		//
		// 2. A LOST RESPONSE on a create that actually committed. Real, and not
		//    fixable here: `item create` has no idempotency key, and titles are
		//    not unique (colliding slugs just get `-2` suffixes,
		//    `store.uniqueSlug`). Nothing auto-retries — the retry would be a
		//    person clicking Create again, seeing the picker's current state —
		//    and the repo's standing rule for the same shape is exactly that
		//    (`item copy` "NEVER retry it automatically"). Filed as IDEA-2880
		//    rather than papered over with a client-side guess — checking for a
		//    same-title item before retrying would rest on the same ranked,
		//    paged, possibly-stale evidence the create row itself rests on, and
		//    would look like a guarantee the client cannot make.
		//
		// 3. Typing a new query, which has been raised twice. The three that ARE fences each stand for an act meaning
		// "not this one": escaping out, choosing a different row, and landing on
		// a different item or workspace. Typing is none of those — it is
		// mid-thought, and the user did explicitly ask for the item now being
		// created. Treating it as a cancel would leave that row orphaned in the
		// target collection with the field still empty, which is worse than a
		// field holding exactly what was asked for.
		//
		// Is the USER still waiting on this specific create?
		//
		// And is the same USER still signed in (BUG-3105)? None of the fences
		// above moves on a sign-out. Two captures, deliberately both: this
		// function's own, and the one ItemPicker hands up through `oncreate`,
		// taken when the create was asked for. Today they are the same tick; the
		// picker's is consumed anyway so that a picker capturing EARLIER (at the
		// menu, as ItemAttachmentStrip's delete does) is honoured here without a
		// second change.
		const isSameIdentity = authStore.identityFence();
		const stillWaiting = () =>
			isSameIdentity() &&
			(pickerIdentity?.() ?? true) &&
			!destroyed &&
			mySeq === relationWrite &&
			ws === wsSlug &&
			collSlug === field.collection &&
			indexStillOurs() &&
			localIndex.scopeEpochFor(ws) === epoch;
		try {
			const item = await api.items.create(ws, collSlug, { title, source: 'web' });
			// The upsert is gated on INDEX IDENTITY only — not on whether the
			// user is still waiting. Those are different questions: a create the
			// user navigated away from still produced a real row that belongs in
			// the index (it is what stops the picker offering to create it
			// again), whereas a create whose workspace was PURGED must not write
			// anything back. `upsert`'s own fenced-id guard cannot help there,
			// because a brand-new id was never in the map to be fenced — the
			// exact gap BUG-2098's comment describes.
			if (indexStillOurs()) localIndex.upsert(ws, item, epoch);
			if (!stillWaiting()) return;
			editingRelation = false;
			commitPicked(item.id);
		} catch (e: any) {
			// The SAME predicate as the success path, not a copy of some of it
			// (codex rounds 4 and 6). A create the user escaped out of, or one
			// belonging to a workspace they have since left or been purged from,
			// must not throw its error over whatever they are looking at now —
			// and the reset half was missing here while the success path had it.
			// One predicate means the two paths cannot drift again.
			if (!stillWaiting()) return;
			// Still here and still waiting: the picker keeps the query, so the
			// user can retry or pick something else; the field's value has not
			// moved.
			toastStore.show(e?.message || 'Failed to create item', 'error');
		}
	}

	// ── Viewport detection ───────────────────────────────────────────────
	// On mobile the absolute-positioned select dropdown can clip at the
	// edge of the properties panel; swap it for a BottomSheet of options
	// (gated on `viewport.isMobile`, TASK-2028). Desktop keeps the inline
	// dropdown with keyboard nav unchanged.
	//
	// If the viewport crosses above mobile while the sheet is open (e.g.
	// rotation), close it so it doesn't spring back on return. Reads the shared
	// breakpoint flag; writes only `dropdownOpen`, so no self-invalidation.
	$effect(() => {
		if (!viewport.isMobile) dropdownOpen = false;
	});

	// ── Date input state ──────────────────────────────────────────────────

	let dateInputEl: HTMLInputElement | undefined = $state(undefined);
	let dateTriggerEl: HTMLButtonElement | undefined = $state(undefined);
	/** True while the hidden date input holds focus (its picker is up). */
	let dateInputFocused = $state(false);

	/**
	 * Open the native date picker (BUG-2858).
	 *
	 * The input is FOCUSED first, synchronously, then `showPicker()` is called.
	 * Both halves are WebKit, read from source:
	 *  - macOS Safari closes its date popover only when a focused inner segment
	 *    field of the input blurs (`DateTimeEditElement::didBlurFromField` →
	 *    `didBlurFromControl`); the popover window has no outside-click monitor,
	 *    and Escape reaches it only when it was opened from the keyboard.
	 *    `showPicker()` never focuses, so a picker opened from this hidden,
	 *    never-focused input could not be dismissed at all.
	 *  - iOS has no `showPicker()` picker (`PageClientImplIOS::createDateTimePicker`
	 *    returns null); its date picker comes only from focus, and only when the
	 *    focus happens inside the tap (`userIsInteracting`). So the focus must stay
	 *    synchronous in this click handler — nothing may be awaited before it.
	 * `showPicker()` can throw (NotAllowedError without activation,
	 * InvalidStateError); focus has already done what it can by then.
	 */
	function openDatePicker() {
		const el = dateInputEl;
		if (!el) return;
		el.focus({ preventScroll: true });
		try {
			el.showPicker();
		} catch {
			// See above: the focus stands on its own.
		}
	}

	/** Escape while the picker's input has focus closes it (macOS Safari: the
	 *  focused segment blurs) and returns focus to the trigger. The pane hosts
	 *  already ignore an Escape whose target is a text-entry input — `date`
	 *  counts — which is what keeps the pane open here (a mutant without the
	 *  `preventDefault` still passes the pane e2e). The key is marked handled
	 *  anyway, for any consumer that asks. */
	function handleDateKeydown(e: KeyboardEvent) {
		if (e.key !== 'Escape') return;
		e.preventDefault();
		dateInputEl?.blur();
		dateTriggerEl?.focus();
	}

	/**
	 * The calendar day a stored date names, as written: `YYYY-MM-DD` itself, or
	 * the day part of an RFC3339 timestamp, which the server also accepts for a
	 * date field (BUG-3225). No timezone conversion: `2026-09-26T23:30:00-05:00`
	 * names the 26th, the day its writer wrote. `null` when the string does not
	 * start with a day.
	 */
	function dateDay(dateStr: string): string | null {
		const m = /^(\d{4}-\d{2}-\d{2})(?:$|T)/.exec(dateStr);
		return m ? m[1] : null;
	}

	function formatDate(dateStr: string): string {
		// Appending a midnight to the whole string turned every RFC3339 value
		// into "Invalid Date" (BUG-3225). And `toLocaleDateString` answers
		// "Invalid Date" rather than throwing, so the old `catch` never ran: an
		// unparseable string now shows as itself.
		const day = dateDay(dateStr);
		const d = day ? new Date(day + 'T00:00:00') : null;
		if (!d || isNaN(d.getTime())) return dateStr;
		return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}

	// ── Select dropdown state ──────────────────────────────────────────────

	let dropdownOpen = $state(false);
	let focusedIndex = $state(-1);
	let triggerEl: HTMLButtonElement | undefined = $state(undefined);
	let dropdownEl: HTMLDivElement | undefined = $state(undefined);

	/** The canonical palette lives in `$lib/utils/fieldColors`; this used to be
	 *  a third copy of `canonicalValueColor` and is now an alias (BUG-3041). */
	const getStatusColor = canonicalValueColor;

	function toggleDropdown() {
		dropdownOpen = !dropdownOpen;
		if (dropdownOpen) {
			focusedIndex = field.options?.indexOf(value) ?? -1;
		}
	}

	function selectOption(opt: string) {
		onchange(opt);
		dropdownOpen = false;
		triggerEl?.focus();
	}

	function handleDropdownKeydown(e: KeyboardEvent) {
		if (!dropdownOpen || !field.options) return;
		const opts = field.options;

		if (e.key === 'ArrowDown') {
			e.preventDefault();
			focusedIndex = (focusedIndex + 1) % opts.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			focusedIndex = (focusedIndex - 1 + opts.length) % opts.length;
		} else if (e.key === 'Enter') {
			e.preventDefault();
			if (focusedIndex >= 0 && focusedIndex < opts.length) {
				selectOption(opts[focusedIndex]);
			}
		} else if (e.key === 'Escape') {
			e.preventDefault();
			dropdownOpen = false;
			triggerEl?.focus();
		}
	}

	// ── Multi-select (IDEA-3223) ──────────────────────────────────────────

	/** The stored selection, or none; a mismatched value never reaches here (`value` is undefined). */
	let storedSelection = $derived(Array.isArray(value) ? (value as string[]) : []);

	/**
	 * What the list shows: the selection we sent while it is outstanding, else
	 * the stored one. Same display hold the checkbox uses (`typedDisplay`), so a
	 * toggle shows at once and a refused write returns to the stored value.
	 */
	let shownSelection = $derived.by<string[]>(() => {
		// `typedDisplay` is the one display hold every typed branch shares, so it
		// can hold another type's text for a moment (a retarget before the
		// subject-change effect clears it). Only an array we wrote is ours.
		if (typedDisplay !== null) {
			try {
				const held = JSON.parse(typedDisplay);
				if (Array.isArray(held)) return held;
			} catch {
				// not ours
			}
		}
		return storedSelection;
	});

	/**
	 * The options, then any stored value that is not one (a renamed or removed
	 * option), so it can still be seen and removed rather than silently kept.
	 */
	let multiChoices = $derived([
		...(field.options ?? []),
		...shownSelection.filter((v) => !(field.options ?? []).includes(v)),
	]);

	function toggleMultiOption(opt: string) {
		// Toggle what the user last SAW, not the prop (the BUG-3047 rule the
		// checkbox follows): the prop lags every write until its round trip
		// lands, so two quick toggles built on it would each drop the other.
		const base: string[] =
			awaitingEcho && awaitingEcho.itemId === itemId && Array.isArray(awaitingEcho.sent)
				? awaitingEcho.sent
				: storedSelection;
		const next = base.includes(opt) ? base.filter((v) => v !== opt) : [...base, opt];
		typedDisplay = JSON.stringify(next);
		sendTyped(next);
	}

	function handleMultiKeydown(e: KeyboardEvent) {
		if (!dropdownOpen) return;
		const opts = multiChoices;
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			focusedIndex = (focusedIndex + 1) % opts.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			focusedIndex = (focusedIndex - 1 + opts.length) % opts.length;
		} else if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault();
			if (focusedIndex >= 0 && focusedIndex < opts.length) toggleMultiOption(opts[focusedIndex]);
		} else if (e.key === 'Escape') {
			e.preventDefault();
			dropdownOpen = false;
			triggerEl?.focus();
		}
	}

	/**
	 * Arrow keys once focus is INSIDE the list (a keyboard user who tabbed into
	 * it): move real focus between the options. Enter and Space are left to the
	 * focused option's own button, so a toggle cannot fire twice.
	 */
	function handleMultiListKeydown(e: KeyboardEvent) {
		const list = e.currentTarget as HTMLElement;
		const opts = [...list.querySelectorAll<HTMLButtonElement>('[role="option"]')];
		const at = opts.indexOf(document.activeElement as HTMLButtonElement);
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			e.preventDefault();
			e.stopPropagation();
			const step = e.key === 'ArrowDown' ? 1 : -1;
			const next = at < 0 ? 0 : (at + step + opts.length) % opts.length;
			focusedIndex = next;
			opts[next]?.focus();
		} else if (e.key === 'Escape') {
			e.preventDefault();
			e.stopPropagation();
			dropdownOpen = false;
			triggerEl?.focus();
		}
	}

	// The desktop dropdown closes through `clickOutside`, which dismisses only on a press that STARTS outside (BUG-3231): a drag begun inside and released outside is not an outside click.
	// On mobile the BottomSheet branch renders instead and owns dismissal.
	const dropdownOutside = {
		onOutside: () => (dropdownOpen = false),
		extra: () => [triggerEl],
	};

	// ── Input handlers ─────────────────────────────────────────────────────
	//
	// Typed inputs (text / number / url) debounce the parent's `onchange`
	// so each keystroke isn't persisted as a separate item update — that
	// produced 30-step keystroke chains in the audit log when a user
	// typed `ui/editor/tiptap` into a `component` field (BUG-1466).
	//
	// Discrete inputs (select / date / checkbox / number ±1 buttons) keep
	// firing immediately — they're single user actions, not typing.
	//
	// On blur we flush any pending save eagerly so tabbing out of the
	// field commits right away instead of waiting out the idle window;
	// likewise on unmount, so navigating away never drops a typed value.
	//
	// Mirrors the markdown content debounce pattern in
	// routes/[username]/[workspace]/[collection]/[slug]/+page.svelte
	// (`contentDebounceTimer`, 1.2s for content); we use 500ms here
	// because fields are small and users expect quicker confirmation
	// than the body editor.

	const TYPING_DEBOUNCE_MS = 500;
	let typingTimer: ReturnType<typeof setTimeout> | undefined;
	let pendingValue: any = undefined;
	// `hasPending` is intentionally a plain let, not $state. It's only
	// read inside imperative handlers (scheduleSave, flushPendingSave,
	// handleNumberStep, the value-track $effect, the unmount cleanup)
	// — never from a template or other reactive context. Making it
	// $state would establish a reactive dependency from the
	// value-track $effect onto hasPending: scheduleSave would set
	// hasPending=true, which would retrigger the $effect, which reads
	// hasPending and clears it — cancelling every keystroke before the
	// debounce can fire. Per Codex review round 4 [P1].
	let hasPending = false;
	// Whose burst the pending value is (BUG-3130). Captured at the burst's FIRST
	// keystroke: the timer and the blur flush both send it later, and a sign-out
	// and a different sign-in while the field stays mounted must not send one
	// user's typing as the next. Plain let for the same reason as `hasPending`.
	let pendingIdentity: (() => boolean) | null = null;

	/**
	 * Drop a pending burst typed under a previous identity — the value AND its
	 * on-screen display, which is that user's text. True when nothing was
	 * dropped.
	 */
	function keepBurstIfOurs(): boolean {
		if (!hasPending || (pendingIdentity?.() ?? true)) return true;
		clearTimeout(typingTimer);
		typingTimer = undefined;
		pendingValue = undefined;
		hasPending = false;
		typedDisplay = null;
		return false;
	}

	/**
	 * What the typed path last SENT, stamped with the item it was sent for, and
	 * held until it comes home (BUG-3039).
	 *
	 * The value-track effect below has to tell "`value` changed because our own
	 * write landed" from "`value` changed because someone else wrote". Those want
	 * opposite answers — keep the newer keystrokes, or drop them — and the effect
	 * used to treat every change as the second, so a response to keystroke N
	 * deleted keystroke N+1 before it was ever sent.
	 *
	 * A plain `let`, not `$state`, for the reason `hasPending` is: the effect
	 * reads it, and making it reactive would have the effect retrigger itself.
	 *
	 * The stamp is the ITEM, not the text, because the text cannot answer the
	 * question: the next item's value can equal what we just sent for this one,
	 * and flushing then would write this item's typing onto that one (the Codex
	 * round 3 [P1] the effect was originally built for).
	 *
	 * RELEASED BY TWO SIGNALS, not one, for the reason the relation hold needed
	 * two (round 8, R8-2): the prop agreeing with what we sent, or the consumer's
	 * own settlement when it gives one. Agreement alone cannot answer for a write
	 * the server REFUSES — that value never comes back, so the stamp would stand
	 * forever and `typedDisplay` with it, pinning rejected text on screen with no
	 * release. An earlier version of this comment claimed nothing was pinned
	 * here. That was wrong, and it was wrong in the direction of defending the
	 * design I had just written (codex round 1, finding 3).
	 */
	let awaitingEcho: { itemId: string | undefined; sent: any } | null = null;

	/**
	 * The text this field is showing while it owns a typed edit the prop has not
	 * caught up with — the display half of BUG-3039.
	 *
	 * Keeping the WRITE was not enough. `value={value ?? ''}` is a one-way
	 * binding, so Svelte re-applies the prop to the DOM input on every change:
	 * the earlier keystroke's echo put `a` back in a box the user had typed `ab`
	 * into, and the character was gone from the screen whether or not the later
	 * write eventually landed.
	 *
	 * This is the same shape as `relationValues`' hold — the last thing WE sent
	 * wins over the prop until the round trip catches up — asked here for one
	 * string instead of a list.
	 *
	 * `$state` because the template reads it, and safe as `$state` for the reason
	 * `hasPending` is NOT: the value-track effect never reads this, so writing it
	 * cannot retrigger the effect. Holding the raw typed STRING rather than the
	 * parsed value keeps a half-typed number (`1.`, `-`) on screen.
	 */
	let typedDisplay = $state<string | null>(null);

	/**
	 * The field-and-mode this editor is currently showing, so a SWAP of subject
	 * clears the typed state the way a change of row does.
	 *
	 * `null` until the value-track effect's first run seeds it. Seeding from the
	 * props HERE would read them at component-init and capture only their initial
	 * values — which is what this needs, but it is also the shape svelte-check
	 * warns about (`state_referenced_locally`), and the effect seeds it for free.
	 */
	let typedSubject: string | null = null;

	/**
	 * Equality as this component means it: the pane stores an empty text field as
	 * `null` and re-props it as `null`, while the input reports `''`.
	 *
	 * Strict otherwise, including across types — if a round trip returns `5` as
	 * `'5'` the echo simply is not recognised and the effect falls back to
	 * dropping the pending edit, which is the direction that was already the
	 * behaviour for everything.
	 */
	function sameTypedValue(a: unknown, b: unknown): boolean {
		const norm = (v: unknown) => (v === null || v === undefined ? '' : v);
		// A multi-select sends an ARRAY, and its echo is a new array: compared by
		// identity it never matched (IDEA-3223).
		if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((v, i) => v === b[i]);
		return norm(a) === norm(b);
	}

	/**
	 * Send a typed value, recording it as the echo we are now waiting for, and
	 * releasing that record when the consumer tells us the write is over.
	 *
	 * The stamp is compared by OBJECT IDENTITY, so an older write's settlement
	 * cannot release a newer write's record — the same thing `holdOrder`'s
	 * tickets do for the relation hold, which needs numbers because its releases
	 * come from several places; here there is one stamp at a time and identity
	 * says it exactly.
	 */
	function sendTyped(v: any) {
		const stamp = { itemId, sent: v };
		awaitingEcho = stamp;
		const settled = settlementOf(onchange(v));
		if (!settled) return;
		const release = () => {
			if (awaitingEcho !== stamp) return; // a newer send owns the record now
			awaitingEcho = null;
			// Only the DISPLAY of this write is released. Anything the user has
			// typed since is newer than the outcome and stays on screen.
			if (!hasPending) typedDisplay = null;
		};
		settled.then(release, release);
	}

	function scheduleSave(next: any) {
		keepBurstIfOurs();
		if (!hasPending) pendingIdentity = authStore.identityFence();
		pendingValue = next;
		hasPending = true;
		clearTimeout(typingTimer);
		typingTimer = setTimeout(() => {
			typingTimer = undefined;
			if (!keepBurstIfOurs()) return;
			const v = pendingValue;
			pendingValue = undefined;
			hasPending = false;
			sendTyped(v);
		}, TYPING_DEBOUNCE_MS);
	}

	function flushPendingSave() {
		if (!keepBurstIfOurs() || !hasPending) return;
		clearTimeout(typingTimer);
		typingTimer = undefined;
		const v = pendingValue;
		pendingValue = undefined;
		hasPending = false;
		sendTyped(v);
	}

	// Cleanup on unmount: drop any pending typed save. We deliberately
	// do NOT flush here:
	//   - When unmount fires because the parent navigated to an item
	//     whose schema doesn't include this field key, the parent's
	//     `item` has already been replaced. Calling onchange now would
	//     route the save through `updateField`, which reads the CURRENT
	//     `item` — writing field-A's typed value into item-B (Codex
	//     review round 3 [P1]).
	//   - The other unmount cases (route teardown, parent destroy) have
	//     ambiguous parent state too.
	// Blur is the supported commit gesture: tabbing out, clicking
	// elsewhere within the page, or the ±1 buttons — all flush via the
	// onblur handler before any teardown. Unmount-without-blur means
	// the user navigated away aggressively; respecting that is safer
	// than a best-effort write to a context we can't validate.
	$effect(() => {
		return () => {
			if (hasPending) {
				clearTimeout(typingTimer);
				typingTimer = undefined;
				pendingValue = undefined;
				hasPending = false;
			}
			awaitingEcho = null;
			typedDisplay = null;
		};
	});

	// Drop pending typed value if the parent updates `value` externally
	// while we have a pending save. The parent could re-prop this
	// FieldEditor mid-edit for legitimate reasons:
	//
	//   - A collab peer / SSE-driven update of the same field on the
	//     same item rebases the prop. The user's in-flight typed
	//     value is no longer the right answer; drop it rather than
	//     silently overwrite the peer edit.
	//   - The parent's own 409 refetch-and-retry does the same.
	//
	// STALE CLAIM REMOVED (BUG-3039, codex round 1 finding 8). This list used to
	// lead with "Navigation to a different item within the same route reuses the
	// component … leaking item A's edit into item B (Codex review round 3 [P1])".
	// That was true when it was written and is not true now: PLAN-2105 /
	// TASK-2112 later put the whole fields panel inside `{#key itemSlug}`
	// (ItemDetail.svelte), whose own comment says the sidebar "remounts on every
	// item switch so a stale field continuation is discarded". An item switch
	// destroys this component; it does not re-prop it. The leak that round 3
	// named is closed structurally, one layer up.
	//
	// Left standing, it cost real work: BUG-3039 was built on it, reported a
	// second hole that does not exist, and had a ruling made on that report
	// before the call site was read.
	//
	// The trade-off: a typed-but-unflushed value is lost when the user
	// navigates within 500ms. Acceptable — they actively navigated, and
	// blur (the usual way to leave an input) already flushes eagerly.
	//
	// It DOES need to distinguish "our save echoing back" from "external update",
	// and the comment that used to stand here said otherwise (BUG-3039). Its
	// reasoning — the timer callback clears `hasPending` before calling onchange,
	// so by the time the echo arrives `hasPending` is already false — holds only
	// while no NEW typing starts between the send and the echo. That window is a
	// 500ms debounce plus a network round trip, which is to say it is the window
	// a person types in: keystroke N's response then cancelled keystroke N+1
	// before it was ever sent, and took it off the screen too.
	//
	// Three cases, and the first two are the ones the original guard exists for:
	//
	//   - the ITEM changed — this component was pointed at another row, so the
	//     pending text belongs to a row we have left. Dropped. Unreachable from
	//     ItemDetail, which remounts instead (see the `itemId` prop); it is the
	//     answer for any caller that does not.
	//   - `value` changed to something we did not send — SSE, a collab peer, the
	//     parent's 409 refetch. Theirs is newer than our unsent edit. Dropped.
	//   - `value` changed to exactly what we last sent for THIS item — our own
	//     write coming home. Kept: the newer keystrokes are the user's current
	//     intent, and nothing has contradicted them.
	$effect(() => {
		void value; // track dependency
		// The SUBJECT, not just the value. A swap of the field this editor is
		// showing, or a flip to readonly, abandons the typed edit as surely as a
		// change of row does — and `typedDisplay` would otherwise survive the swap
		// and paint the old text over the new field's value (codex round 1,
		// finding 5). Read before the early returns so they are dependencies on
		// every path.
		const subject = `${field.key}\u0000${field.type}\u0000${readonly ? 'ro' : 'rw'}`;
		if (subject !== typedSubject) {
			// The FIRST run seeds `typedSubject` and takes this branch too. That is
			// a no-op rather than a special case: effects run before the user can
			// type, so there is never anything to clear at that point. A `seeding`
			// guard here produced a mutant nothing could kill, which is the tell
			// that it was deciding nothing.
			typedSubject = subject;
			clearTimeout(typingTimer);
			typingTimer = undefined;
			pendingValue = undefined;
			hasPending = false;
			awaitingEcho = null;
			typedDisplay = null;
			return;
		}
		// …and the retarget. NO TEST CAN DISTINGUISH THIS LINE — removing it
		// survives the matrix (D3 on the BUG-3039 trail) — for two independent
		// reasons, and both are worth stating because either alone would be a
		// reason to delete it. First, in this Svelte version an effect re-runs on
		// every assignment of a prop it reads, equal or not (the same behaviour
		// round 8 hit as a HAZARD above), so reassigning `value` already re-runs
		// this. Second, ItemDetail remounts rather than retargeting, so the case
		// this covers is not reachable from the only caller that has items.
		// It stays for the same reason the prop does: the logic depends on
		// `itemId`, and declaring that beats resting on an incidental re-run and
		// a `{#key}` in another file.
		void itemId;
		if (!hasPending) {
			if (!awaitingEcho) {
				// Nothing of ours is outstanding, so the prop is the truth again.
				//
				// This clear is UNREACHABLE today and kept as the invariant it
				// states: `typedDisplay` is only ever meaningful while something is
				// pending or a send is outstanding, and every path that drops
				// `awaitingEcho` already drops it — so no test can kill removing
				// this line (D8 on the BUG-3039 trail). It stays because it is what
				// makes that invariant true by construction rather than by tracing
				// five paths, and a future path that sets the display without a
				// send would otherwise pin text with nothing to release it.
				typedDisplay = null;
				return;
			}
			if (awaitingEcho.itemId === itemId && sameTypedValue(awaitingEcho.sent, value)) {
				// Our newest send came home.
				awaitingEcho = null;
				typedDisplay = null;
				return;
			}
			// Our newest send is still outstanding and this is not it: an OLDER
			// send's echo, or an outside write. Indistinguishable, and both want
			// the same answer — keep showing what we sent, because it is later
			// than either. Releasing here showed the older value until the newer
			// echo arrived (codex round 1, findings 2 and 4). The settlement
			// signal in `sendTyped` is what guarantees this ends even if our echo
			// never comes.
			return;
		}
		if (awaitingEcho && awaitingEcho.itemId === itemId && sameTypedValue(awaitingEcho.sent, value)) {
			// Consumed: an echo answers for exactly one write, so a LATER outside
			// change equal to the same text is an outside change and drops as one.
			awaitingEcho = null;
			return;
		}
		clearTimeout(typingTimer);
		typingTimer = undefined;
		pendingValue = undefined;
		hasPending = false;
		awaitingEcho = null;
		typedDisplay = null;
	});

	function handleTextInput(e: Event) {
		const target = e.target as HTMLInputElement;
		typedDisplay = target.value;
		scheduleSave(target.value);
	}

	function handleNumberInput(e: Event) {
		const target = e.target as HTMLInputElement;
		// Recorded even when the text does not parse: `1.` and `-` are on their
		// way to a number and must not be rewritten under the cursor. Nothing is
		// SENT for them, which is unchanged.
		typedDisplay = target.value;
		if (target.value === '') { scheduleSave(null); return; }
		const num = Number(target.value);
		if (!isNaN(num)) scheduleSave(num);
	}

	function handleNumberStep(delta: number) {
		// ±1 buttons are a discrete action that supersedes any in-flight
		// typed value. Compute the new value from the pending typed value
		// (if any) BEFORE clearing the timer — `value` is the parent
		// prop and would lag a freshly-typed pending number until the
		// flush round-trips back. Per Codex review round 1 [P1].
		// Three bases, most-recent-first. `value` is the parent prop and LAGS
		// anything we have sent but not seen come home, so stepping from it turns
		// `5` then `+1` into `oldValue + 1` and the typed 5 is gone (codex round 1,
		// finding 1 — pre-existing, and reachable now that `awaitingEcho` records
		// the missing middle case).
		// A burst typed under a previous identity is not a base to step from.
		keepBurstIfOurs();
		const base = hasPending
			? Number(pendingValue) || 0
			: awaitingEcho
				? Number(awaitingEcho.sent) || 0
				: Number(value) || 0;
		clearTimeout(typingTimer);
		typingTimer = undefined;
		pendingValue = undefined;
		hasPending = false;
		typedDisplay = String(base + delta);
		sendTyped(base + delta);
	}

	function handleDateInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onchange(target.value || null);
	}

	function handleCheckboxToggle() {
		// Toggle what the user last SAW, not the prop (BUG-3047). The prop lags
		// every write until its round trip lands, so two quick clicks negated the
		// same old value, both sent it, and the field ended where it started: one
		// click lost. The base is the newest value we sent, for this item, still
		// outstanding; the prop only when nothing of ours is.
		//
		// Same record and same display hold the number step uses (`sendTyped` /
		// `typedDisplay`), so the checkbox inherits every release path they have:
		// its own echo coming home, the write settling (a REFUSED toggle returns
		// to the stored value), a subject swap, unmount.
		const base =
			awaitingEcho && awaitingEcho.itemId === itemId ? !!awaitingEcho.sent : !!value;
		const next = !base;
		typedDisplay = String(next);
		sendTyped(next);
	}

	/** What the switch shows: the toggle we sent while it is outstanding, else the prop. */
	let shownChecked = $derived(typedDisplay !== null ? typedDisplay === 'true' : !!value);

	// ── Scroll focused option into view ────────────────────────────────────

	$effect(() => {
		if (dropdownOpen && focusedIndex >= 0 && dropdownEl) {
			const items = dropdownEl.querySelectorAll('[role="option"]');
			const item = items[focusedIndex] as HTMLElement | undefined;
			item?.scrollIntoView({ block: 'nearest' });
		}
	});

</script>


{#snippet relationChip(raw: string)}
	<!--
		One chip for ONE reference, four states. The invariant across all of
		them: a raw item ID never reaches the user. Before this branch existed,
		the readonly arm was `{value ?? '—'}`, which rendered the UUID verbatim.

		Takes the reference as a parameter rather than closing over `value` so
		a `multi_relation` renders N of these with no second chip
		implementation (U4/W7).
	-->
	{@const row = relationRowFor(raw)}
	{@const state = relationStateFor(raw)}
	{@const ref = relationRefFor(row)}
	{@const href = relationHrefFor(row)}
	{#if state === 'empty'}
		<span class="relation-empty">—</span>
	{:else if state === 'live' && href && row}
		<a
			class="relation-chip link-target"
			{href}
			onclick={(e) => handleRelationClick(e, row)}
		>
			{#if ref}<span class="relation-ref">{ref}</span>{/if}
			<span class="relation-title">{row.title}</span>
		</a>
	{:else if state === 'live'}
		<!-- Resolved, but no route to build (no `username`): still never the id. -->
		<span class="relation-chip">
			{#if ref}<span class="relation-ref">{ref}</span>{/if}
			<span class="relation-title">{row?.title}</span>
		</span>
	{:else if state === 'deleted'}
		<span class="relation-chip is-deleted" title="This item has been deleted.">
			{#if ref}<span class="relation-ref">{ref}</span>{/if}
			<span class="relation-title">{row?.title}</span>
			<span class="relation-note">(deleted)</span>
		</span>
	{:else if isRelationValueStoredAsText(raw)}
		<!--
			Stored as TEXT, not as an id (BUG-3014): a title a bundle import
			carried verbatim, or legacy free text. It was never a reference, so
			the unresolved chip overstates it; show the text and say what it
			is. Not a claim that it is broken: it may name a live item, and
			resolving it here would make the answer depend on the reader. The
			server marks the same values `stored_as_text` on `relation_targets`.
		-->
		<span class="relation-chip is-text" title="Stored as text, not as a reference to an item.">
			<span class="relation-title">{raw.trim()}</span>
			<span class="relation-note">(text, not a reference)</span>
		</span>
	{:else}
		<!--
			An id that does not resolve to an item the viewer can see. The wording
			is neutral because the miss may be a live target in a collection this
			member cannot see, as well as a dangling id (BUG-3013; see
			UNRESOLVED_LABEL). Text values took the branch above.
		-->
		<span class="relation-chip is-unresolved" title={UNRESOLVED_TITLE}>
			<span class="relation-note">{UNRESOLVED_LABEL}</span>
		</span>
	{/if}
{/snippet}

{#snippet relationChips()}
	<!--
		The field's whole value, read-only: one chip for a `relation`, N in
		order for a `multi_relation`, an em-dash for either when empty. The
		em-dash is rendered HERE rather than left to the chip's own empty arm so
		an empty LIST says the same thing an empty scalar does — a list of zero
		chips would otherwise render as nothing at all.
	-->
	{#if relationValues.length === 0}
		<span class="relation-empty">—</span>
	{:else if isMultiRelation}
		<span class="relation-chips">
			{#each relationValues as raw, i (raw + '@' + i)}
				{@render relationChip(raw)}
			{/each}
		</span>
	{:else}
		{@render relationChip(relationValues[0])}
	{/if}
{/snippet}

{#if !shape.ok && !replacing}
	<div class="field-mismatch">
		<span class="mismatch-raw">{rawText(storedValue)}</span>
		<span class="mismatch-note">Doesn't match the field type ({field.type})</span>
		{#if !readonly}
			<button type="button" class="mismatch-action" onclick={() => (replacingKey = mismatchKey)}>Replace</button>
		{/if}
	</div>

{:else if readonly}
	<!--
		Display-only mode (PLAN-1100 / TASK-1105). No inputs, no dropdowns,
		no mutation handlers — value rendered with the same visual language
		as the editor's idle state. Maps each field type to a non-interactive
		representation; the empty case renders an em-dash to match how the
		select/date triggers display "no value".
	-->
	{#if field.type === 'select'}
		<div class="readonly-display">
			{#if value && getStatusColor(value)}
				<span class="color-dot" style:background={getStatusColor(value)}></span>
			{/if}
			<span class="select-label">{value ? formatLabel(value) : '—'}</span>
		</div>
	{:else if field.type === 'checkbox'}
		<div class="readonly-display">
			<span>{value ? 'Yes' : 'No'}</span>
		</div>
	{:else if field.type === 'date'}
		<div class="readonly-display">
			<span>{value ? formatDate(value) : '—'}</span>
		</div>
	{:else if field.type === 'number'}
		<div class="readonly-display">
			<span>{value ?? '—'}{value != null && field.suffix ? field.suffix : ''}</span>
		</div>
	{:else if field.type === 'url'}
		<div class="readonly-display">
			{#if value}
				<a href={value} target="_blank" rel="noopener noreferrer" class="url-readonly">{value}</a>
			{:else}
				<span>—</span>
			{/if}
		</div>
	{:else if field.type === 'json'}
		<div class="readonly-display">
			<span>{value === undefined || value === null ? '—' : JSON.stringify(value)}</span>
		</div>
	{:else if field.type === 'multi_select'}
		<div class="readonly-display">
			<span>{storedSelection.length ? storedSelection.map(formatLabel).join(', ') : '—'}</span>
		</div>
	{:else if isRelation}
		<div class="readonly-display">{@render relationChips()}</div>
	{:else}
		<div class="readonly-display">
			<span>{value ?? '—'}</span>
		</div>
	{/if}

{:else if field.type === 'select'}
	{#snippet selectOptions()}
		{#if field.options}
			{#each field.options as option, i (option)}
				<button
					class="select-option"
					class:selected={option === value}
					class:focused={i === focusedIndex}
					type="button"
					role="option"
					aria-selected={option === value}
					onclick={() => selectOption(option)}
					onmouseenter={() => (focusedIndex = i)}
				>
					{#if getStatusColor(option)}
						<span class="color-dot" style:background={getStatusColor(option)}></span>
					{/if}
					<span>{formatLabel(option)}</span>
				</button>
			{/each}
		{/if}
	{/snippet}

	<!-- Custom select dropdown -->
	<div class="select-wrapper">
		<button
			bind:this={triggerEl}
			class="select-trigger"
			type="button"
			aria-label={ariaLabel}
			aria-haspopup="listbox"
			aria-expanded={dropdownOpen}
			onclick={toggleDropdown}
			onkeydown={handleDropdownKeydown}
		>
			{#if value && getStatusColor(value)}
				<span class="color-dot" style:background={getStatusColor(value)}></span>
			{/if}
			<span class="select-label">
				{value ? formatLabel(value) : '\u2014'}
			</span>
			<svg class="select-chevron" width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
				<path d="M3 4.5L6 7.5L9 4.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
			</svg>
		</button>

		{#if viewport.isMobile && dropdownOpen}
			<!--
				Mobile: full-width BottomSheet of options, titled with the
				field label (e.g. "Set status"). Gated on `dropdownOpen`
				(gate-on-open pattern) so the sheet + its global keydown
				listener isn't mounted per idle FieldEditor instance.
			-->
			<BottomSheet
				open={dropdownOpen}
				onclose={() => (dropdownOpen = false)}
				title="Set {field.label.toLowerCase()}"
			>
				<div class="select-sheet-body" role="listbox" aria-label="{field.label} options">
					{@render selectOptions()}
				</div>
			</BottomSheet>
		{:else if dropdownOpen && field.options}
			<div
				bind:this={dropdownEl}
				use:clickOutside={dropdownOutside}
				class="select-dropdown"
				role="listbox"
				aria-label="{field.label} options"
			>
				{@render selectOptions()}
			</div>
		{/if}
	</div>

{:else if field.type === 'checkbox'}
	<!-- Toggle switch -->
	<button
		class="toggle"
		class:on={shownChecked}
		type="button"
		role="switch"
		aria-checked={shownChecked}
		aria-label={field.label}
		onclick={handleCheckboxToggle}
	>
		<span class="toggle-knob"></span>
	</button>

{:else if field.type === 'date'}
	<!-- Custom date picker -->
	<div class="date-wrapper">
		<button
			bind:this={dateTriggerEl}
			class="select-trigger date-trigger"
			type="button"
			aria-label={ariaLabel}
			onclick={openDatePicker}
		>
			{#if value}
				<span class="date-label">{formatDate(value)}</span>
				<span
					class="clear-btn"
					role="button"
					tabindex="0"
					onclick={(e) => { e.stopPropagation(); onchange(null); }}
					onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); onchange(null); } }}
				>&#x2715;</span>
			{:else}
				<span class="date-placeholder">Pick a date...</span>
			{/if}
			<svg class="select-chevron" width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">
				<rect x="2" y="3" width="10" height="9" rx="1.5" stroke="currentColor" stroke-width="1.2" />
				<line x1="2" y1="6" x2="12" y2="6" stroke="currentColor" stroke-width="1.2" />
				<line x1="5" y1="1.5" x2="5" y2="4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
				<line x1="9" y1="1.5" x2="9" y2="4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
			</svg>
		</button>
		<!-- Hidden from assistive tech only while it does NOT hold focus: a
		     focused aria-hidden element is an a11y error, and once focused it
		     is the control being operated, so it carries the field's label. -->
		<input
			bind:this={dateInputEl}
			class="date-hidden-input"
			type="date"
			value={value ? (dateDay(value) ?? '') : ''}
			onchange={handleDateInput}
			onkeydown={handleDateKeydown}
			onfocus={() => (dateInputFocused = true)}
			onblur={() => (dateInputFocused = false)}
			tabindex={-1}
			aria-hidden={dateInputFocused ? undefined : 'true'}
			aria-label={ariaLabel ?? (field.label || field.key)}
		/>
	</div>

{:else if field.type === 'number'}
	<!-- Custom number input with +/- buttons.
	     onmousedown={preventDefault} on the buttons keeps the input
	     focused across the click — without this, the natural focus
	     transfer fires the input's onblur (→ flushPendingSave clears
	     hasPending) BEFORE the button's onclick runs, so
	     handleNumberStep would read stale `value` from the parent prop.
	     Per Codex review round 2 [P1]. -->
	<div class="number-wrapper">
		<button
			class="number-btn"
			type="button"
			tabindex={-1}
			aria-label="Decrease"
			onmousedown={(e) => e.preventDefault()}
			onclick={() => handleNumberStep(-1)}
		>
			<svg width="10" height="10" viewBox="0 0 10 10" fill="none" aria-hidden="true">
				<line x1="2" y1="5" x2="8" y2="5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
			</svg>
		</button>
		<input
			class="number-input"
			type="text"
			aria-label={ariaLabel}
			inputmode="numeric"
			value={typedDisplay ?? value ?? ''}
			oninput={handleNumberInput}
			onblur={flushPendingSave}
			placeholder="—"
		/>
		{#if field.suffix}
			<span class="number-suffix">{field.suffix}</span>
		{/if}
		<button
			class="number-btn"
			type="button"
			tabindex={-1}
			aria-label="Increase"
			onmousedown={(e) => e.preventDefault()}
			onclick={() => handleNumberStep(1)}
		>
			<svg width="10" height="10" viewBox="0 0 10 10" fill="none" aria-hidden="true">
				<line x1="2" y1="5" x2="8" y2="5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
				<line x1="5" y1="2" x2="5" y2="8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
			</svg>
		</button>
	</div>

{:else if field.type === 'url'}
	<!-- URL input with link icon -->
	<div class="url-wrapper">
		<input
			class="field-input url-input"
			type="url"
			aria-label={ariaLabel}
			value={typedDisplay ?? value ?? ''}
			oninput={handleTextInput}
			onblur={flushPendingSave}
			placeholder="https://..."
		/>
		{#if value}
			<a
				class="url-open"
				href={value}
				target="_blank"
				rel="noopener noreferrer"
				title="Open link"
			>
				<svg width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
					<path d="M5 1H2.5A1.5 1.5 0 001 2.5v7A1.5 1.5 0 002.5 11h7A1.5 1.5 0 0011 9.5V7" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
					<path d="M7 1h4v4M11 1L5.5 6.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round" />
				</svg>
			</a>
		{/if}
	</div>

{:else if isRelation && relationEditable}
	<div class="relation-editor">
		{#if isMultiRelation}
			<!--
				A LIST, so the affordances are per-ELEMENT (Remove) plus one for
				the list (Add). The scalar's Change/Clear pair does not
				translate: "Change" would have to mean "replace everything",
				which is not what a user pointing at one chip is asking for.

				The picker's open/closed rule is the SCALAR'S, deliberately
				unchanged — open when the field is empty or when the user asked
				for it, closed once a choice lands. An empty list auto-opening
				is the same trade the scalar comment above `editingRelation`
				argues for: nothing to show means nothing to show INSTEAD of a
				search box.
			-->
			{#each relationValues as raw, i (raw + '@' + i)}
				<div class="relation-row">
					{@render relationChip(raw)}
					<button
						type="button"
						class="relation-action"
						onclick={() => removeRelationAt(i)}
					>
						Remove
					</button>
				</div>
			{/each}
			<!--
				DUPLICATE PREVENTION, in its entirety: the picker is told to hide
				what the field already references, so the click the server would
				refuse is never offered. The server REFUSES a duplicate
				(`RelationTargetDuplicate`) rather than de-duplicating, so that
				refusal — not a silent one-copy write — is what stands behind
				this if the exclusion is ever dropped.

				The exclusion set is the RAW stored elements, and that is
				sufficient rather than lazy: the write door canonicalises every
				relation value to its target's UUID (`ResolveRelationReferents`),
				so a stored element IS the id even when the caller typed a ref or
				a title. Excluding raw strings therefore also covers the element
				`localIndex` cannot resolve — the one whose target the picker can
				still offer, because while the index is cold it searches the
				SERVER, over rows that were never in the index.

				A resolved-id set was written alongside this and removed: with
				canonicalisation it is the same set, and no mutant could tell the
				two apart. If stored elements ever stop being canonical ids, THIS
				is the line that stops being sufficient.
			-->
			{#if editingRelation || relationValues.length === 0}
				<ItemPicker
					wsSlug={wsSlug!}
					collection={field.collection}
					label={ariaLabel ?? `Search ${field.label || field.key}`}
					placeholder="Search…"
					autofocus={editingRelation}
					excludeIds={relationValues}
					onselect={pickRelation}
					oncreate={canCreateInTarget ? createRelationTarget : undefined}
					createLabel={targetCollection?.name}
					oncancel={relationValues.length === 0 ? undefined : cancelRelationEdit}
				/>
			{:else}
				<div class="relation-row">
					<button type="button" class="relation-action" onclick={() => (editingRelation = true)}>
						+ Add
					</button>
				</div>
			{/if}
		{:else if relationValues.length > 0 && !editingRelation}
			<div class="relation-row">
				{@render relationChip(relationValues[0])}
				<button type="button" class="relation-action" onclick={() => (editingRelation = true)}>
					Change
				</button>
				<button type="button" class="relation-action" onclick={clearRelation}>Clear</button>
			</div>
		{:else}
			<ItemPicker
				wsSlug={wsSlug!}
				collection={field.collection}
				label={ariaLabel ?? `Search ${field.label || field.key}`}
				placeholder="Search…"
				autofocus={editingRelation}
				onselect={pickRelation}
				oncreate={canCreateInTarget ? createRelationTarget : undefined}
				createLabel={targetCollection?.name}
				oncancel={relationValues.length === 0 ? undefined : cancelRelationEdit}
			/>
		{/if}
	</div>

{:else if isRelation}
	<!--
		Editable in principle, but not here: no workspace slug or no target
		collection. See the `wsSlug` prop doc — an unscoped picker in the
		cross-workspace copy dialog would offer the wrong workspace's items and
		look authoritative. Read-only is the honest state until TASK-2869.
	-->
	<div class="readonly-display" title="This relation can't be set from here yet.">
		{@render relationChips()}
	</div>

{:else if field.type === 'json'}
	<!-- JSON fields need a structured editor; the generic FieldEditor
	     intentionally exposes only a read-only summary so a plain text
	     input can't corrupt the stored value with a "[]" string instead
	     of an actual array. Dedicated editors (e.g. the playbook editor
	     that owns `arguments`) render their own UI for these. -->
	<div class="readonly-display" title="Edit this JSON field from its dedicated editor.">
		<span>{value === undefined || value === null ? '—' : JSON.stringify(value)}</span>
	</div>

{:else if field.type === 'multi_select'}
	<!-- Multi-select (IDEA-3223). It used to fall to the text input, which
	     wrote a STRING the server refuses for a multi_select. Each option is a
	     toggle; the list stays open so several can be set in one visit. -->
	{#snippet multiOptions()}
		{#each multiChoices as option, i (option)}
			<button
				class="select-option"
				class:selected={shownSelection.includes(option)}
				class:focused={i === focusedIndex}
				type="button"
				role="option"
				aria-selected={shownSelection.includes(option)}
				onclick={() => toggleMultiOption(option)}
				onmouseenter={() => (focusedIndex = i)}
			>
				<span class="multi-check" aria-hidden="true">{shownSelection.includes(option) ? '✓' : ''}</span>
				{#if getStatusColor(option)}
					<span class="color-dot" style:background={getStatusColor(option)}></span>
				{/if}
				<span>{formatLabel(option)}</span>
			</button>
		{/each}
	{/snippet}

	<div class="select-wrapper">
		<button
			bind:this={triggerEl}
			class="select-trigger"
			type="button"
			aria-label={ariaLabel}
			aria-haspopup="listbox"
			aria-expanded={dropdownOpen}
			onclick={toggleDropdown}
			onkeydown={handleMultiKeydown}
		>
			<span class="select-label">
				{shownSelection.length ? shownSelection.map(formatLabel).join(', ') : '\u2014'}
			</span>
			<svg class="select-chevron" width="12" height="12" viewBox="0 0 12 12" fill="none" aria-hidden="true">
				<path d="M3 4.5L6 7.5L9 4.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
			</svg>
		</button>

		{#if viewport.isMobile && dropdownOpen}
			<BottomSheet
				open={dropdownOpen}
				onclose={() => (dropdownOpen = false)}
				title="Set {field.label.toLowerCase()}"
			>
				<div class="select-sheet-body" role="listbox" aria-multiselectable="true" aria-label="{field.label} options" tabindex="-1" onkeydown={handleMultiListKeydown}>
					{@render multiOptions()}
				</div>
			</BottomSheet>
		{:else if dropdownOpen && multiChoices.length}
			<div
				bind:this={dropdownEl}
				use:clickOutside={dropdownOutside}
				class="select-dropdown"
				role="listbox"
				aria-multiselectable="true"
				aria-label="{field.label} options"
				tabindex="-1"
				onkeydown={handleMultiListKeydown}
			>
				{@render multiOptions()}
			</div>
		{/if}
	</div>

{:else}
	<!-- Text fallback -->
	<input
		class="field-input"
		type="text"
		aria-label={ariaLabel}
		value={typedDisplay ?? value ?? ''}
		oninput={handleTextInput}
		onblur={flushPendingSave}
	/>
{/if}
{#if replacing}
	<button type="button" class="mismatch-action" onclick={() => (replacingKey = null)}>Keep the stored value</button>
{/if}

<style>
	/* ── Shape mismatch (BUG-3052 unit 2) ─────────────────────────────── */

	.field-mismatch {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-1) var(--space-2);
		padding: var(--space-1) var(--space-2);
		min-height: 30px;
		font-size: 0.88em;
	}

	.mismatch-raw {
		font-family: var(--font-mono);
		color: var(--text-primary);
		overflow-wrap: anywhere;
	}

	.mismatch-note {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.mismatch-action {
		font-size: 0.85em;
		color: var(--accent-blue);
		background: none;
		border: none;
		padding: 0;
		cursor: pointer;
	}

	.mismatch-action:hover {
		text-decoration: underline;
	}

	/* ── Readonly display ─────────────────────────────────────────────── */

	.readonly-display {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-1) var(--space-2);
		min-height: 30px;
		font-size: 0.88em;
		color: var(--text-primary);
	}

	.url-readonly {
		color: var(--accent-blue);
		text-decoration: none;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.url-readonly:hover {
		text-decoration: underline;
	}

	/* ── Relation chip (TASK-2868) ────────────────────────────────────── */

	.relation-editor {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 0;
		font-size: 0.88em;
	}

	.relation-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}

	.relation-action {
		flex-shrink: 0;
		padding: 0;
		border: none;
		background: none;
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
	}

	.relation-action:hover {
		color: var(--text-primary);
		text-decoration: underline;
	}

	.relation-chip {
		display: inline-flex;
		align-items: baseline;
		gap: var(--space-2);
		min-width: 0;
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
		font-size: 0.88em;
	}

	a.relation-chip:hover {
		text-decoration: underline;
	}

	.relation-ref {
		flex-shrink: 0;
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.94em;
	}

	.relation-title {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* Both non-live states read as muted rather than alarming: a dangling
	   reference is information, not an error the user caused. */
	.relation-chip.is-deleted,
	.relation-chip.is-unresolved {
		color: var(--text-muted);
	}

	.relation-note {
		flex-shrink: 0;
		font-style: italic;
	}

	/* Stored text (BUG-3014): the value itself reads normally, since it is
	   what the field holds; only the note is muted. */
	.relation-chip.is-text .relation-note {
		color: var(--text-muted);
	}

	.relation-empty {
		color: var(--text-muted);
	}

	/* A read-only multi_relation is N chips on one line, wrapping — the
	   editable form gets a row each because each row carries its own Remove. */
	.relation-chips {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}

	/* ── Shared input styles ──────────────────────────────────────────── */

	.field-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		min-height: 30px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		outline: none;
		transition: border-color 0.15s;
		box-sizing: border-box;
	}

	.field-input:hover {
		border-color: var(--border);
	}

	.field-input:focus {
		border-color: var(--accent-blue);
	}

	.field-input::placeholder {
		color: var(--text-muted);
	}

	/* ── Date picker ─────────────────────────────────────────────────── */

	.date-wrapper {
		position: relative;
		width: 100%;
	}

	.date-trigger {
		position: relative;
	}

	.date-label {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.date-placeholder {
		flex: 1;
		color: var(--text-muted);
	}

	.date-hidden-input {
		position: absolute;
		inset: 0;
		opacity: 0;
		width: 100%;
		height: 100%;
		cursor: pointer;
		pointer-events: none;
		/* iOS zooms the page onto a focused input under 16px (BUG-2858: the
		   input is now focused to open its picker). Invisible either way. */
		font-size: 16px;
	}

	/* ── Number input ────────────────────────────────────────────────── */

	.number-wrapper {
		display: flex;
		align-items: center;
		width: 100%;
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		transition: border-color 0.15s;
		overflow: hidden;
	}

	.number-wrapper:hover {
		border-color: var(--border);
	}

	.number-wrapper:focus-within {
		border-color: var(--accent-blue);
	}

	.number-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		min-height: 30px;
		padding: 0;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		flex-shrink: 0;
		transition: color 0.1s, background 0.1s;
	}

	.number-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.number-btn:active {
		background: var(--bg-active);
	}

	.number-input {
		flex: 1;
		min-width: 0;
		padding: var(--space-1) var(--space-1);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: transparent;
		border: none;
		outline: none;
		text-align: center;
		box-sizing: border-box;
	}

	.number-input::placeholder {
		color: var(--text-muted);
	}

	.number-suffix {
		font-size: 0.82em;
		color: var(--text-muted);
		padding-right: var(--space-2);
		pointer-events: none;
		user-select: none;
		flex-shrink: 0;
	}

	/* ── Select dropdown ──────────────────────────────────────────────── */

	.select-wrapper {
		position: relative;
		width: 100%;
	}

	.select-trigger {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		min-height: 30px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		cursor: pointer;
		outline: none;
		transition: border-color 0.15s;
		text-align: left;
		box-sizing: border-box;
	}

	.select-trigger:hover {
		border-color: var(--border);
	}

	.select-trigger:focus {
		border-color: var(--accent-blue);
	}

	.select-label {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.select-chevron {
		flex-shrink: 0;
		color: var(--text-muted);
		transition: transform 0.15s;
	}

	.color-dot {
		display: inline-block;
		width: 8px;
		height: 8px;
		border-radius: 50%;
		flex-shrink: 0;
	}

	.select-dropdown {
		position: absolute;
		top: calc(100% + 4px);
		left: 0;
		right: 0;
		z-index: 50;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) 0;
		max-height: 200px;
		overflow-y: auto;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
	}

	.select-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		box-sizing: border-box;
	}

	.select-option:hover,
	.select-option.focused {
		background: var(--bg-hover);
	}

	.select-option.selected {
		background: var(--bg-active);
	}

	/* ── Mobile sheet body — roomier rows for touch ──────────────────── */

	.select-sheet-body {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-2) var(--space-3);
	}

	.select-sheet-body .select-option {
		padding: var(--space-3);
		font-size: 1em;
		border-radius: var(--radius-sm);
	}

	/* ── Checkbox toggle switch ───────────────────────────────────────── */

	.toggle {
		position: relative;
		width: 36px;
		height: 20px;
		padding: 0;
		background: var(--bg-tertiary);
		border: none;
		border-radius: 10px;
		cursor: pointer;
		transition: background-color 0.15s;
		flex-shrink: 0;
	}

	.toggle.on {
		background: var(--accent-blue);
	}

	.toggle-knob {
		position: absolute;
		top: 2px;
		left: 2px;
		width: 16px;
		height: 16px;
		background: white;
		border-radius: 50%;
		transition: transform 0.15s;
		pointer-events: none;
	}

	.toggle.on .toggle-knob {
		transform: translateX(16px);
	}

	.clear-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: pointer;
		padding: 2px;
		border-radius: var(--radius-sm);
		flex-shrink: 0;
	}

	.clear-btn:hover {
		color: var(--accent-orange);
		background: var(--bg-hover);
	}

	/* ── URL input ────────────────────────────────────────────────────── */

	.url-wrapper {
		position: relative;
		display: flex;
		align-items: center;
		width: 100%;
	}

	.url-input {
		padding-right: calc(var(--space-2) + 20px);
	}

	.url-open {
		position: absolute;
		right: var(--space-2);
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--text-muted);
		padding: 2px;
		border-radius: var(--radius-sm);
		transition: color 0.1s;
	}

	.url-open:hover {
		color: var(--accent-blue);
	}
</style>
