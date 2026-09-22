<script lang="ts">
	import { page } from '$app/state';
	import { onDestroy, onMount } from 'svelte';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { titleLimitError } from '$lib/items/titleLimit';
	import { authStore } from '$lib/stores/auth.svelte';
	import { itemUrlId, isAgentCollection } from '$lib/types';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import type { Item, Collection, RoleBoardLane, AgentRole } from '$lib/types';
	import ItemCard from '$lib/components/collections/ItemCard.svelte';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';

	/**
	 * BUG-3084: the page's signed-in-identity fence. Surface 2; the reasoning
	 * and the two-capture design are `[collection]/+page.svelte`'s, and the
	 * conditional-reload argument that makes a fence necessary at all is the
	 * same — `routes/+layout.svelte` does NOT reload on a sign-IN from
	 * anonymous and does NOT reload on sign-out, so this page can outlive an
	 * identity change with work in flight.
	 *
	 * WHAT IS DIFFERENT HERE, and it is why this surface went before the larger
	 * ones: this page does not merely REPORT stale results, it COMPOSES WRITES
	 * from state captured under the previous identity. `handleDndFinalize`
	 * builds `update.assigned_user_id = currentUserId`, and `currentUserId` is
	 * read from `/auth/session` by `loadData`. After an identity change, a drag
	 * assigns items to the PREVIOUS user's id — a write that is wrong in the
	 * database rather than merely misleading on screen, and one the server
	 * cannot refuse, since the new user may legitimately assign to anyone.
	 *
	 * So the check on that path sits before the WRITE, not merely before the
	 * commit that follows it. Everywhere else the rule is the usual one: after
	 * each await, immediately before anything that writes page state, writes a
	 * store, shows a toast, or navigates.
	 *
	 * `identityHeld(captured)` takes a handler's own ENTRY capture, because the
	 * page's load epoch is RE-STAMPED by `loadData` and a handler comparing
	 * against it can be defeated by a concurrent load setting it to the value
	 * the handler is about to see.
	 *
	 * `pageIdentityHeld()` answers the other question — does this PAGE still
	 * belong to the signed-in user — and BOTH DRAG HANDLERS need it, which is
	 * not obvious and was not believed until a test said so. An entry capture
	 * asks whether the identity moved since the work STARTED; these handlers'
	 * problem is that their work starts correctly and reads state loaded
	 * EARLIER. A drag begun after an identity change passes every entry capture
	 * — it is the current epoch — and still writes `currentUserId` and a lane
	 * order belonging to the previous session. Both questions, or the gap is
	 * exactly the one this surface was prioritised for.
	 */
	// `$state` rather than a plain `let`, because `pageIdentityHeld()` is read
	// from an `$effect` below as well as from event handlers (codex round 2
	// [P1]). A plain `let` is not tracked, so that effect would never re-run
	// when the epoch is re-stamped and would keep whichever answer it saw first.
	let identityEpochAtLoad = $state(authStore.identityEpoch);

	/** The epoch as of now — captured at a handler's entry, before its awaits. */
	function captureIdentity(): number {
		return authStore.identityEpoch;
	}

	function identityHeld(captured: number): boolean {
		return authStore.identityEpoch === captured;
	}

	function pageIdentityHeld(): boolean {
		return authStore.identityEpoch === identityEpochAtLoad;
	}

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	// Role-board mutations gate on owner role (PLAN-1100 / TASK-1108):
	// role create/edit/delete are owner-only on the server, and item drag
	// across lanes is per-item edit which we proxy via owner-or-editor —
	// the server enforces per-item, but svelte-dnd-action only supports
	// zone-level dragDisabled (same constraint as TASK-1106 ListView).
	let isOwner = $derived(workspaceStore.isOwner);
	// Lane reorder is editor+ on the server (handleRoleBoardLaneReorder
	// uses requireMinRole "editor"). Role create/edit/delete remain
	// owner-only.
	let canReorderLanes = $derived(
		workspaceStore.currentRole === 'owner' || workspaceStore.currentRole === 'editor'
	);
	// canEditAnyItem mirrors the server's grant-aware reorder permission:
	// a viewer/guest with even one CollectionGrant.edit or ItemGrant.edit
	// can mutate items in this view (server enforces per-item; we just
	// avoid hiding their drag affordance globally).
	let canEditAnyItem = $derived.by(() => {
		const role = workspaceStore.currentRole;
		if (role === 'owner' || role === 'editor') return true;
		const m = workspaceStore.currentMembership;
		if (!m) return false;
		if (m.collection_grants.some((g) => g.permission === 'edit')) return true;
		if (m.item_grants.some((g) => g.permission === 'edit')) return true;
		return false;
	});

	// Data
	let lanes = $state<RoleBoardLane[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Scroll position restoration (BUG-1425). Lanes render as a board, so
	// page-level scroll is dominantly vertical — board-internal horizontal
	// scroll is out of scope here (same constraint as the collection-page
	// board view).
	const scrollRestoration = createScrollRestoration({
		// `loading` flips true on workspace change. Length gate omitted
		// (Codex P2 round 2).
		ready: () => !loading,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	// Highlight: dim cards not assigned to current user
	let highlightMine = $state(false);

	// New item modal state
	let newItemOpen = $state(false);
	let newItemTitleInput = $state<HTMLInputElement | null>(null);
	let newItemCollectionSlug = $state('');
	let newItemTitle = $state('');
	let newItemSaving = $state(false);

	// Eligible collections for the "+ New" item flow. Filter to collections
	// the user can actually create items in — handleCreateItem requires
	// collection-level edit on the server, so an item-only edit grant
	// doesn't qualify (Codex round 2).
	let eligibleCollections = $derived(
		collectionStore.collections.filter(
			(c) => !isAgentCollection(c)
				&& workspaceStore.canEditCollection(c.id)
		)
	);

	function openNewItem() {
		newItemCollectionSlug = '';
		newItemTitle = '';
		newItemOpen = true;
	}

	function closeNewItem() {
		newItemOpen = false;
		newItemCollectionSlug = '';
		newItemTitle = '';
	}

	function selectCollection(slug: string) {
		newItemCollectionSlug = slug;
		// Focus the title input after selection (it renders once a collection is picked)
		requestAnimationFrame(() => {
			newItemTitleInput?.focus();
		});
	}

	async function submitNewItem() {
		if (!newItemTitle.trim() || !newItemCollectionSlug || newItemSaving) return;
		// BUG-3115: refuse a too-long title before sending; the form keeps it.
		const limitError = titleLimitError(newItemTitle.trim());
		if (limitError) {
			toastStore.show(limitError, 'error');
			return;
		}
		// `pageIdentityHeld()` as well as an entry capture, for the reason
		// `handleDndFinalize` gives (codex round 2 [P1]). The collection and the
		// title were chosen by whoever filled this form in, against the
		// collection list the PREVIOUS identity loaded. The modal is mounted
		// outside the `{#if loading}` branch, so it survives the whole reload
		// and a submit afterwards is the current epoch — every entry capture
		// passes.
		if (!pageIdentityHeld()) {
			closeNewItem();
			return;
		}
		const epochAtEntry = captureIdentity();
		newItemSaving = true;
		try {
			await api.items.create(wsSlug, newItemCollectionSlug, {
				title: newItemTitle.trim()
			});
			if (!identityHeld(epochAtEntry)) return;
			closeNewItem();
			await loadData();
		} catch (err) {
			// The toast reaches the GLOBAL store, so it outlives this page's
			// state entirely (BUG-3084).
			if (!identityHeld(epochAtEntry)) return;
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				// BUG-3115: this used to be console-only, so a refusal looked
				// like a button that did nothing.
				toastStore.show((err as Error)?.message || 'Failed to create item', 'error');
			}
		} finally {
			// FENCED, unlike `loadData`'s busy flag, and the asymmetry is the
			// point (codex round 4 [P2]). `loading` is cleared unconditionally
			// because nothing else clears it and pinning the board at the
			// skeleton is the worse failure. `newItemSaving` IS cleared by
			// `resetTransientState()` on every identity change, so a stale
			// continuation clearing it again can only re-enable a form the NEW
			// user is already using — permitting a duplicate submit.
			if (identityHeld(epochAtEntry)) newItemSaving = false;
		}
	}

	function handleNewItemKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && !e.shiftKey) {
			e.preventDefault();
			submitNewItem();
		}
	}

	// Role editing modal state
	let roleDialogOpen = $state(false);
	let dialogMode = $state<'edit' | 'create'>('create');
	let editingRoleId = $state<string | null>(null);
	let editName = $state('');
	let editDescription = $state('');
	let editIcon = $state('');
	let editTools = $state('');

	function openEditModal(role: AgentRole) {
		dialogMode = 'edit';
		editingRoleId = role.id;
		editName = role.name;
		editDescription = role.description;
		editIcon = role.icon;
		editTools = role.tools;
		roleDialogOpen = true;
	}

	function openCreateModal() {
		dialogMode = 'create';
		editingRoleId = null;
		editName = '';
		editDescription = '';
		editIcon = '';
		editTools = '';
		roleDialogOpen = true;
	}

	function closeModal() {
		roleDialogOpen = false;
	}
	let currentUserId = $state('');

	// Reorder lanes: unassigned first, then roles in order
	let orderedLanes = $derived.by(() => {
		const unassigned = lanes.filter((l) => !l.role);
		const assigned = lanes.filter((l) => l.role);
		return [...unassigned, ...assigned];
	});

	let totalItems = $derived(orderedLanes.reduce((sum, lane) => sum + lane.items.length, 0));

	// Drag-and-drop state
	const flipDurationMs = 200;
	const touchDragDelayMs = 500;
	let isDragging = $state(false);

	// Lane (header) drag-and-drop state
	let draggedLaneKey = $state<string | null>(null);
	let dragOverLaneKey = $state<string | null>(null);

	function handleLaneDragStart(e: DragEvent, key: string) {
		if (key === '__unassigned') { e.preventDefault(); return; }
		draggedLaneKey = key;
		if (e.dataTransfer) {
			e.dataTransfer.effectAllowed = 'move';
			e.dataTransfer.setData('text/plain', key);
		}
	}

	function handleLaneDragOver(e: DragEvent, key: string) {
		if (!draggedLaneKey || key === draggedLaneKey || key === '__unassigned') return;
		e.preventDefault();
		dragOverLaneKey = key;
	}

	function handleLaneDragLeave() {
		dragOverLaneKey = null;
	}

	async function handleLaneDrop(e: DragEvent, key: string) {
		e.preventDefault();
		// Same reason as `handleDndFinalize`: the order this persists is derived
		// from `lanes`, loaded under the previous identity. A drag beginning
		// after the change would otherwise write the previous user's board
		// arrangement (BUG-3084).
		if (!pageIdentityHeld()) return;
		const epochAtEntry = captureIdentity();
		if (!draggedLaneKey || key === '__unassigned') { draggedLaneKey = null; dragOverLaneKey = null; return; }

		// Reorder the assigned lanes (skip unassigned)
		const assignedLanes = lanes.filter((l) => l.role);
		const srcIdx = assignedLanes.findIndex((l) => l.role!.id === draggedLaneKey);
		const dstIdx = assignedLanes.findIndex((l) => l.role!.id === key);

		if (srcIdx >= 0 && dstIdx >= 0 && srcIdx !== dstIdx) {
			const [moved] = assignedLanes.splice(srcIdx, 1);
			// After removing from srcIdx, indices shift left — adjust if moving forward
			const insertIdx = srcIdx < dstIdx ? dstIdx - 1 : dstIdx;
			assignedLanes.splice(insertIdx, 0, moved);

			// Rebuild lanes with new order
			const unassigned = lanes.filter((l) => !l.role);
			lanes = [...unassigned, ...assignedLanes];

			// Persist new sort order
			const updates = assignedLanes.map((lane, i) => ({
				role_id: lane.role!.id,
				sort_order: i
			}));

			try {
				await api.agentRoles.reorderLanes(wsSlug, updates);
			} catch (err) {
				console.error('Failed to persist lane order:', err);
				// The recovery reload is a commit too: it repaints the board for
				// whoever is signed in NOW from a failure the previous user's
				// drag caused (BUG-3084).
				if (!identityHeld(epochAtEntry)) return;
				await loadData();
			}
		}

		if (!identityHeld(epochAtEntry)) return;
		draggedLaneKey = null;
		dragOverLaneKey = null;
	}

	function handleLaneDragEnd() {
		draggedLaneKey = null;
		dragOverLaneKey = null;
	}

	// Mutable lane data for DnD — keyed by role ID (or '__unassigned')
	let laneData = $state<Record<string, Item[]>>({});

	// Sync from orderedLanes when not dragging — and only while this page holds
	// the signed-in identity (codex round 2 [P1]). `handleDndFinalize` mutates
	// `lanes` OPTIMISTICALLY before its request resolves; if the identity moves
	// during the `await loadData()` on its failure path, that load's own fence
	// returns early and leaves the optimistic value in place, and releasing
	// `isDragging` afterwards re-enables this effect to paint the previous
	// identity's board. The fence here is what stops that, rather than trying to
	// make the release conditional — the release must be unconditional or the
	// board pins on stale lane data for ever, which is the failure #1372 shipped.
	$effect(() => {
		if (!isDragging && pageIdentityHeld()) {
			const data: Record<string, Item[]> = {};
			for (const lane of orderedLanes) {
				const key = lane.role?.id ?? '__unassigned';
				data[key] = [...lane.items];
			}
			laneData = data;
		}
	});

	function laneKey(lane: RoleBoardLane): string {
		return lane.role?.id ?? '__unassigned';
	}

	function handleDndConsider(key: string, e: CustomEvent<DndEvent<Item>>) {
		laneData[key] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleDndFinalize(key: string, e: CustomEvent<DndEvent<Item>>) {
		// BOTH QUESTIONS, and the entry capture alone is not enough — which the
		// behavioural suite proved rather than the reasoning (BUG-3084).
		//
		// `pageIdentityHeld()` first: this handler composes its write from
		// `currentUserId` and `lanes`, both loaded by `loadData` under whoever
		// was signed in THEN. A drag that STARTS after an identity change is a
		// new user's intent carried out with the previous user's data, so an
		// entry capture is the current epoch, passes, and the stale assignee is
		// written anyway. Nothing about "has the identity moved since this work
		// started" can see that; the question is whether the PAGE still belongs
		// to the signed-in user.
		// NOTHING IS TOUCHED ON A LOST-IDENTITY EXIT (codex round 4 [P2]).
		//
		// Round 1 had these paths set `isDragging = false` on the way out, on
		// the grounds that a fence returning without clearing it pins the board
		// on stale lane data for ever. That was correct THEN and is wrong now:
		// `resetTransientState()` clears it from the identity listener, which
		// runs synchronously on the epoch bump and therefore always BEFORE a
		// stale continuation resumes. So the flag is already released — and
		// clearing it again here would cancel a drag the NEW user has since
		// started.
		//
		// The general rule, which is what changed: a continuation that has lost
		// the identity may not write SHARED interaction state at all. Someone
		// else owns it now. The latch that rule used to risk is closed by the
		// listener owning the reset, not by each stale handler cleaning up after
		// itself.
		if (!pageIdentityHeld()) return;
		const epochAtEntry = captureIdentity();
		const finalItems = e.detail.items.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]);
		laneData[key] = finalItems;

		// Keep isDragging true until lanes state is updated,
		// so the $effect doesn't overwrite laneData from stale orderedLanes.

		const { id: itemId, trigger } = e.detail.info;

		// Check for cross-lane move
		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = orderedLanes.flatMap((l) => l.items).find((i) => i.id === itemId);
			const oldKey = originalItem ? (originalItem.agent_role_id ?? '__unassigned') : key;

			if (originalItem && oldKey !== key) {
				// Cross-lane: update the item's role
				const newRoleId = key === '__unassigned' ? null : key;
				const targetRole = orderedLanes.find((l) => laneKey(l) === key)?.role ?? null;

				const updatedItem = { ...originalItem,
					agent_role_id: newRoleId,
					agent_role_name: targetRole?.name ?? '',
					agent_role_slug: targetRole?.slug ?? '',
					agent_role_icon: targetRole?.icon ?? '',
				};

				if (!originalItem.assigned_user_id && currentUserId && newRoleId) {
					updatedItem.assigned_user_id = currentUserId;
				}

				lanes = lanes.map((lane) => {
					const lk = lane.role?.id ?? '__unassigned';
					if (lk === oldKey) {
						return { ...lane, items: lane.items.filter((i) => i.id !== itemId) };
					}
					if (lk === key) {
						return { ...lane, items: [...lane.items.filter((i) => i.id !== itemId), updatedItem] };
					}
					return lane;
				});

				try {
					const update: Record<string, any> = {};
					if (key === '__unassigned') {
						update.clear_agent_role = true;
					} else {
						update.agent_role_id = key;
						if (!originalItem.assigned_user_id && currentUserId) {
							update.assigned_user_id = currentUserId;
						}
					}
					// BEFORE THE WRITE, not merely before the commit that follows
					// it (BUG-3084). `currentUserId` comes from the previous
					// session's `/auth/session`, so issuing this PERSISTS a wrong
					// assignee rather than just displaying one — and the server
					// cannot refuse it, since the new user may legitimately
					// assign to anyone. This is the only check on this page whose
					// position is chosen by what the request CONTAINS rather than
					// by what lands after it.
					// See the lost-identity rule at the top of this handler. This
					// exit used to release `isDragging` here, with a comment
					// explaining that nothing else would; round 3 made that
					// false by giving the reset to the identity listener, and
					// this guard's own assertion is what noticed the comment was
					// still describing the old arrangement.
					if (!identityHeld(epochAtEntry)) return;
					await api.items.update(wsSlug, originalItem.id, update);
					// See the lost-identity rule at the top of this handler.
					if (!identityHeld(epochAtEntry)) return;
				} catch (err) {
					console.error('Failed to update role:', err);
					// See the lost-identity rule at the top of this handler.
					if (!identityHeld(epochAtEntry)) return;
					await loadData();
					// GATED, like every other continuation (codex round 5 [P2]).
					// The check above happens BEFORE this await, so it says
					// nothing about an identity change during the recovery load
					// — and releasing the flag afterwards would cancel a drag the
					// NEW user has since started. Same rule as the lost-identity
					// exits above; this one sits after an await rather than
					// inside a guard arm, which is why the first version of the
					// source assertion did not see it.
					if (identityHeld(epochAtEntry)) isDragging = false;
					return;
				}
			}
		}

		// Always persist sort order for all items in this lane (covers both
		// within-lane reorder and cross-lane moves)
		const reorderUpdates = finalItems.map((item, index) => ({
			item_id: item.id,
			role_sort_order: index
		}));

		// Optimistic: update lanes state with new sort orders BEFORE releasing isDragging
		// See the lost-identity rule at the top of this handler.
		if (!identityHeld(epochAtEntry)) return;
		lanes = lanes.map((lane) => {
			if (laneKey(lane) !== key) return lane;
			return { ...lane, items: finalItems.map((item, index) => ({ ...item, role_sort_order: index })) };
		});

		// Now safe to release — lanes has the correct data for the $effect to sync from
		isDragging = false;

		try {
			await api.agentRoles.reorder(wsSlug, reorderUpdates);
			if (!identityHeld(epochAtEntry)) return;
		} catch (err) {
			console.error('Failed to persist sort order:', err);
		}
	}

	/**
	 * RE-LOAD ON AN IDENTITY CHANGE (codex round 1 [P1], BUG-3084).
	 *
	 * `pageIdentityHeld()` compares against `identityEpochAtLoad`, which only
	 * `loadData` re-stamps — and `loadData` runs on mount and after mutations,
	 * never because the identity moved. On an anonymous -> signed-in
	 * transition, which `routes/+layout.svelte` deliberately does NOT reload
	 * for, this page stays mounted with a stale epoch and every
	 * `pageIdentityHeld()` is false FOR EVER: both drag handlers go silently
	 * inert and never recover.
	 *
	 * So the stale-data condition the fence detects has to be RESOLVED, not
	 * refused indefinitely. Re-loading re-stamps the epoch and replaces the
	 * previous session's board with this one's, which is the state the fence
	 * was protecting against showing in the first place.
	 *
	 * SAFE ORDER, and it is the store's contract rather than luck: the epoch is
	 * bumped BEFORE listeners run (`notifyIdentityChange`), and that function's
	 * own comment places the obligation on listeners to load for the identity
	 * signed in NOW rather than re-issuing anything captured earlier. `loadData`
	 * reads `wsSlug` from the live route and captures nothing, so it satisfies
	 * that.
	 */
	/**
	 * Every `$state` on this page that holds TRANSIENT INTERACTION STATE —
	 * something a user gesture started and a later gesture or response would
	 * finish. Distinct from the page's DATA (`lanes`, `currentUserId`), which
	 * `loadData` replaces, and from its IDENTITY bookkeeping
	 * (`identityEpochAtLoad`), which only `loadData` may write.
	 *
	 * The source guard holds this function against the file's own `$state`
	 * declarations, so a new piece of interaction state cannot arrive without
	 * someone dispositioning it here.
	 */
	function resetTransientState() {
		closeModal();
		closeNewItem();
		newItemSaving = false;
		isDragging = false;
		draggedLaneKey = null;
		dragOverLaneKey = null;
		laneData = {};
	}

	const stopIdentityWatch = authStore.onIdentityChange(() => {
		// NO CLEAR HERE, and that is a measured decision rather than an omission
		// (BUG-3084 checkpoint 14). #1374 clears surface 1's rendered state
		// before re-loading, and reconciling the two shapes is what this branch
		// owed. They reconcile on the PROPERTY, not on the code: a recovery must
		// never leave a fence vouching for state the recovery has not replaced.
		//
		// Surface 1 holds that property by clearing, because its markup keeps
		// the previous collection on screen for the length of the round-trip.
		// This page holds it at the source instead: `loadData` re-stamps
		// `identityEpochAtLoad` only AFTER the new data has landed, so every
		// `pageIdentityHeld()` between the change and the reload answers false
		// and both drag handlers refuse. Clearing on top of that was tried and
		// removed — mutants M3 and M4 (drop `currentUserId = null`, drop
		// `lanes = []`) SURVIVED the whole suite, which is the evidence that
		// neither line was doing anything here, not a gap in the instruments.
		//
		// KNOWN LATCH, stated because it is the #1372 failure mode and nothing
		// below asserts it: if `wsSlug` were ever falsy this skips the reload,
		// nothing re-stamps, and the fence stays shut for ever. It is a route
		// parameter on a route that cannot match without it, so there is no
		// reachable path — but a future refactor that makes it optional owes a
		// re-stamp on this branch.
		// EVERY piece of transient interaction state, reset together (codex
		// round 3). Rounds 2 and 3 each found members of one class one at a time
		// — an open modal, a stuck `newItemSaving`, a stuck `isDragging`, stale
		// lane-drag styling — which is CONVE-35's signal that the loop was
		// measuring the enumeration rather than the code. So it is enumerated
		// here instead, in one place, with the source guard asserting the list
		// against the file's own `$state` declarations.
		//
		// Two distinct reasons, and both are needed:
		//
		//   DISCLOSURE. The modals are mounted OUTSIDE the `{#if loading}`
		//   branch, so unlike the board they do not disappear for the reload: an
		//   open role-edit form keeps showing the previous identity's role name,
		//   description and tools to whoever is signed in now. Their handlers are
		//   separately fenced against WRITING that state; this is the other half,
		//   and a fence stops the write, it does not stop the reading.
		//
		//   LATCHING. `isDragging` is the sharp one. The `{#if loading}` branch
		//   destroys the dnd zones without dispatching `finalize`, so a drag in
		//   progress when the identity moves leaves `isDragging` true with no
		//   handler left to clear it — and the `$effect` that repopulates
		//   `laneData` is disabled while it is true. The board then never
		//   repopulates and the page shows the previous identity's lane data for
		//   ever. That is the #1372 failure mode exactly: a guard that latches
		//   into the safe state and cannot leave it. `newItemSaving` latches the
		//   same way, pinning the create form disabled behind a request nobody
		//   can observe.
		resetTransientState();
		if (wsSlug) void loadData();
	});
	onDestroy(stopIdentityWatch);

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
		uiStore.onNavigate();
		loadData();
	});

	// Bumped by every `loadData()`. Without it an OLDER load's unconditional
	// `finally` clears `loading` while a newer one is still in flight, and the
	// markup re-renders the board over whatever `lanes` currently holds — which
	// during an identity-change reload is the previous identity's (codex round 2
	// [P1]). The settings page solves the same problem the same way; this page
	// had no generation guard at all.
	let loadGen = 0;

	async function loadData() {
		const epochAtEntry = captureIdentity();
		const myLoad = ++loadGen;
		loading = true;
		error = '';
		try {
			const [boardResult, session] = await Promise.all([
				api.agentRoles.board(wsSlug),
				api.auth.session()
			]);
			if (!identityHeld(epochAtEntry)) return;
			// GENERATION-GATED as well as identity-fenced (codex round 4 [P2]).
			// The two questions are different and neither implies the other: the
			// fence asks whether the SIGNED-IN USER changed, the generation asks
			// whether a NEWER LOAD for the same user already landed. `loadGen`
			// was introduced one round earlier gating only `loading`, which is a
			// half-measure that reads as though the race were handled — a slow
			// first load could still overwrite a newer post-mutation load after
			// the newer one had already painted the board.
			if (myLoad !== loadGen) return;
			lanes = boardResult.lanes;
			if (!session.authenticated || !session.user) {
				// CLEARED, not left alone (codex round 3 [P2]). A load that comes
				// back unauthenticated used to fall past this and re-stamp the
				// epoch over the PREVIOUS user's id, so the page then vouched for
				// itself while `handleDndFinalize` still had someone to assign to.
				currentUserId = '';
			} else {
				// THE SHARPEST WRITE ON THIS PAGE. `currentUserId` is what
				// `handleDndFinalize` stamps into `assigned_user_id`, so a value
				// landing here from the previous session's `/auth/session` is a
				// wrong id that later writes will persist (BUG-3084).
				currentUserId = session.user.id;
			}
			// RE-STAMPED HERE, AFTER the data it vouches for has landed — not
			// before the await (BUG-3084 checkpoint 14).
			//
			// `pageIdentityHeld()` is the question "does this page's DATA belong
			// to the signed-in user", and `handleDndFinalize` composes a write
			// from that data. Re-stamping at the TOP of the load answers yes for
			// the whole round-trip, while `currentUserId` and `lanes` are still
			// the previous session's — so the guard vouches for data it has not
			// yet replaced. That is the mirror image of the #1372 regression:
			// there the reference was never re-stamped and the fence latched
			// shut; here it was re-stamped too early and the fence opened onto
			// stale state. One line, two failure modes, and the correct position
			// is bounded on both sides — after the state is replaced, and on
			// every path that leaves the page usable.
			identityEpochAtLoad = epochAtEntry;
		} catch (err) {
			if (!identityHeld(epochAtEntry)) return;
			if (myLoad !== loadGen) return;
			error = err instanceof Error ? err.message : 'Failed to load role board';
			// CLEARED HERE, because the re-stamp below makes the fence vouch for
			// whatever is left. On the success path the awaited values replace
			// these; on this path nothing does.
			lanes = [];
			currentUserId = '';
			// RE-STAMPED ON THE ERROR PATH TOO, and the reason is NOT the one
			// this comment used to give (codex round 2 [P2]). It said the
			// listener had cleared `currentUserId` and `lanes` first, so a
			// failed reload left nothing of the previous identity to protect.
			// That was true when it was written and false two commits later,
			// when the clears were removed — authored and falsified inside one
			// unit, which is the CONVE-23 sweep failing on my own change.
			//
			// The actual reason is a trade, stated as one: NOT re-stamping pins
			// the page inert for ever on a transient network error, which is the
			// outage #1374 was opened to repair, and that is the worse failure.
			// What re-stamping costs is that the page-level fence answers true
			// over state the failed reload never replaced — so the error path
			// clears that state itself, below, rather than relying on anyone
			// else having done it.
			identityEpochAtLoad = epochAtEntry;
		} finally {
			// NOT identity-fenced: this must run on every exit path or the board
			// is pinned at "Loading…" on a page that did not remount, and the
			// flag discloses nothing about either user. GENERATION-gated though,
			// so only the NEWEST load may declare the page loaded.
			if (myLoad === loadGen) loading = false;
		}
	}

	async function saveRole() {
		if (!editName.trim()) return;
		// `pageIdentityHeld()` as well as an entry capture, and for
		// `handleDndFinalize`'s reason (codex round 2 [P1]). This handler writes to
		// `editingRoleId`, which `openEditModal` took from `lanes` — a role id
		// belonging to the board the PREVIOUS identity loaded. The modals are
		// mounted outside the `{#if loading}` branch, so unlike the drag handlers
		// they stay usable for the whole reload.
		// A click that happens AFTER an identity change is the current epoch, so
		// every entry capture passes; the question that catches it is whether
		// the PAGE still belongs to the signed-in user.
		if (!pageIdentityHeld()) {
			closeModal();
			return;
		}
		const epochAtEntry = captureIdentity();
		try {
			if (dialogMode === 'edit' && editingRoleId) {
				await api.agentRoles.update(wsSlug, editingRoleId, {
					name: editName.trim(),
					description: editDescription.trim(),
					icon: editIcon.trim(),
					tools: editTools.trim()
				});
				if (!identityHeld(epochAtEntry)) return;
			} else {
				await api.agentRoles.create(wsSlug, {
					name: editName.trim(),
					description: editDescription.trim(),
					icon: editIcon.trim(),
					tools: editTools.trim()
				});
				if (!identityHeld(epochAtEntry)) return;
			}
			// PER BRANCH above rather than once here, and the difference is not
			// cosmetic: only one branch runs, so a single check after the
			// if/else guards whichever await happened — but it reads as though
			// one check covers two awaits, and the next person to add a third
			// branch inherits that reading. Checking where the await is keeps
			// the rule "after each await" literally true (BUG-3084).
			closeModal();
			await loadData();
		} catch (e) {
			if (!identityHeld(epochAtEntry)) return;
			console.error('Failed to save role:', e);
		}
	}

	async function deleteRole() {
		if (!editingRoleId) return;
		if (!confirm(`Delete role "${editName}"? Items assigned to this role will become unassigned.`)) return;
		// AFTER the confirm, deliberately: `confirm` blocks the main thread, so
		// no identity change can be observed while it is open, and capturing
		// before it would be the same value. Capturing after keeps the rule
		// "at entry, before the first await" literally true for every handler
		// on the page, which is what the source guard enumerates (BUG-3084).
		// `pageIdentityHeld()` as well as an entry capture, and for
		// `handleDndFinalize`'s reason (codex round 2 [P1]). It DELETES `editingRoleId`, taken
		// by `openEditModal` from the previous identity's board — the most
		// destructive write on the page.
		// A click that happens AFTER an identity change is the current epoch, so
		// every entry capture passes; the question that catches it is whether
		// the PAGE still belongs to the signed-in user.
		if (!pageIdentityHeld()) {
			closeModal();
			return;
		}
		const epochAtEntry = captureIdentity();
		try {
			await api.agentRoles.delete(wsSlug, editingRoleId);
			if (!identityHeld(epochAtEntry)) return;
			closeModal();
			await loadData();
		} catch (e) {
			if (!identityHeld(epochAtEntry)) return;
			console.error('Failed to delete role:', e);
		}
	}

	function collectionForItem(item: Item): Collection | undefined {
		return collectionStore.collections.find(c => c.slug === item.collection_slug);
	}
</script>

<svelte:head>
	<title>Role Board - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="role-board-page">
	<!-- Layout-only wrapper: keeps the header non-shrinking inside the page's
	     column flex and carries the mobile padding the page previously put on
	     .page-header (the page itself drops to padding: 0 on mobile). -->
	<div class="header-wrap">
		<PageHeader title="Role Board" icon="🎭" count={loading ? undefined : totalItems}>
			{#snippet actions()}
				<button
					class="toggle-btn"
					class:active={highlightMine}
					onclick={() => highlightMine = !highlightMine}
				>
					Mine
				</button>
				<!-- "+ New" only when there's at least one collection the user can
				     create items in (eligibleCollections is now canEditCollection-filtered). -->
				{#if eligibleCollections.length > 0}
					<button class="new-item-btn" onclick={openNewItem}>+ New</button>
				{/if}
			{/snippet}
		</PageHeader>
	</div>


	<!-- Role edit/create modal -->
<!-- New Item Modal -->
<Modal
	open={newItemOpen}
	onclose={closeNewItem}
	placement="center"
	maxWidth="400px"
	labelledby="new-item-dialog-title"
	--modal-border="none"
	--modal-shadow="0 16px 48px rgba(0, 0, 0, 0.3)"
>
	<div class="dialog-content new-item-content">
		{#if !newItemCollectionSlug}
			<div class="dialog-header">
				<h2 id="new-item-dialog-title">New Item</h2>
				<button class="dialog-close" onclick={closeNewItem}>✕</button>
			</div>
			<div class="collection-grid">
				{#each eligibleCollections as coll (coll.id)}
					<button class="collection-pick" onclick={() => selectCollection(coll.slug)}>
						<span class="collection-pick-icon">{coll.icon || '📦'}</span>
						<span class="collection-pick-name">{coll.name}</span>
					</button>
				{/each}
			</div>
		{:else}
			{@const selectedColl = eligibleCollections.find(c => c.slug === newItemCollectionSlug)}
			<div class="dialog-header">
				<button class="back-btn" onclick={() => { newItemCollectionSlug = ''; newItemTitle = ''; }} title="Back">←</button>
				<h2 id="new-item-dialog-title">New {selectedColl?.icon} {selectedColl?.name?.replace(/s$/, '') ?? 'Item'}</h2>
				<button class="dialog-close" onclick={closeNewItem}>✕</button>
			</div>
			<div class="new-item-form">
				<input
					bind:this={newItemTitleInput}
					class="new-item-title-input"
					type="text"
					placeholder="Title…"
					bind:value={newItemTitle}
					onkeydown={handleNewItemKeydown}
				/>
				<button
					class="new-item-create-btn"
					disabled={!newItemTitle.trim() || newItemSaving}
					onclick={submitNewItem}
				>
					{newItemSaving ? 'Creating…' : 'Create'}
				</button>
			</div>
		{/if}
	</div>
</Modal>

<Modal
	open={roleDialogOpen}
	onclose={closeModal}
	placement="center"
	maxWidth="520px"
	labelledby="roles-dialog-title"
	--modal-bg="var(--bg-primary)"
	--modal-border="none"
	--modal-shadow="0 20px 60px rgba(0, 0, 0, 0.3)"
>
	<div class="dialog-content">
		<div class="dialog-header">
			<h2 id="roles-dialog-title">{dialogMode === 'edit' ? 'Edit Role' : 'New Role'}</h2>
			<button class="dialog-close" onclick={closeModal}>✕</button>
		</div>

		<div class="dialog-body">
			<div class="role-edit-form">
				<div class="role-field-group">
					<label class="role-field-label" for="role-name">Icon & Name</label>
					<div class="role-edit-row">
						<EmojiPickerButton bind:value={editIcon} placeholder="🔨" size="md" />
						<input id="role-name" class="role-input" type="text" bind:value={editName} placeholder="Role name" />
					</div>
				</div>
				<div class="role-field-group">
					<label class="role-field-label" for="role-description">Description</label>
					<input id="role-description" class="role-input" type="text" bind:value={editDescription} placeholder="What does this role do?" />
				</div>
				<div class="role-field-group">
					<label class="role-field-label" for="role-tools">Tools</label>
					<input id="role-tools" class="role-input" type="text" bind:value={editTools} placeholder="e.g. Claude Code + Sonnet 4.6" />
				</div>
			</div>
		</div>

		<div class="dialog-footer">
			{#if dialogMode === 'edit'}
				<button class="role-btn role-btn-danger" onclick={deleteRole}>Delete Role</button>
				<div class="dialog-footer-right">
					<button class="role-btn" onclick={closeModal}>Cancel</button>
					<button class="role-btn role-btn-save" disabled={!editName.trim()} onclick={saveRole}>Save</button>
				</div>
			{:else}
				<div></div>
				<div class="dialog-footer-right">
					<button class="role-btn" onclick={closeModal}>Cancel</button>
					<button class="role-btn role-btn-save" disabled={!editName.trim()} onclick={saveRole}>Create</button>
				</div>
			{/if}
		</div>
	</div>
</Modal>

	{#if loading}
		<div class="skeleton-board">
			{#each Array(4) as _, i (i)}
				<div class="skeleton-lane">
					<div class="skeleton-lane-header"></div>
					{#each Array(3) as _, j (j)}
						<div class="skeleton-card"></div>
					{/each}
				</div>
			{/each}
		</div>
	{:else if error}
		<EmptyState icon="!" title="Failed to load" message={error}>
			{#snippet actions()}
				<Button variant="secondary" onclick={loadData}>Retry</Button>
			{/snippet}
		</EmptyState>
	{:else if orderedLanes.length === 0}
		{#if highlightMine}
			<EmptyState
				icon="👤"
				title="No items assigned to you"
				message="Turn off &quot;My Work&quot; to see all items, or assign items to yourself from the item detail page."
			/>
		{:else}
			<EmptyState
				icon="🎭"
				title="No roles configured"
				message="Agent roles let you organize work by what kind of thinking it requires — planning, implementing, reviewing, etc."
			>
				{#snippet actions()}
					{#if isOwner}
						<Button variant="secondary" onclick={openCreateModal}>Create your first role</Button>
					{/if}
				{/snippet}
			</EmptyState>
		{/if}
	{:else}
		<div class="lanes-container">
			{#each orderedLanes as lane (lane.role?.id ?? '__unassigned')}
				{@const isUnassigned = !lane.role}
				{@const laneId = lane.role?.id ?? '__unassigned'}
				<div
					class="lane"
					class:unassigned={isUnassigned}
					class:dragging-source={draggedLaneKey === laneId}
					class:drag-over-left={dragOverLaneKey === laneId}
				>
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="lane-header"
						draggable={!isUnassigned && canReorderLanes}
						ondragstart={canReorderLanes ? (e) => handleLaneDragStart(e, laneId) : undefined}
						ondragover={canReorderLanes ? (e) => handleLaneDragOver(e, laneId) : undefined}
						ondragleave={canReorderLanes ? handleLaneDragLeave : undefined}
						ondrop={canReorderLanes ? (e) => handleLaneDrop(e, laneId) : undefined}
						ondragend={canReorderLanes ? handleLaneDragEnd : undefined}
					>
						<div class="lane-title-row">
							{#if lane.role}
								{#if canReorderLanes}
									<span class="lane-drag-handle" title="Drag to reorder">⠿</span>
								{/if}
								<span class="lane-icon">{lane.role.icon || '&#129302;'}</span>
								<span class="lane-name">{lane.role.name}</span>
							{:else}
								<span class="lane-name unassigned-name">Unassigned</span>
							{/if}
							<span class="lane-count">{lane.items.length}</span>
							{#if lane.role && isOwner}
								<button class="lane-edit-btn" title="Edit role" onclick={() => lane.role && openEditModal(lane.role)}>✎</button>
							{/if}
						</div>
						{#if lane.role?.tools}
							<div class="lane-tools">{lane.role.tools}</div>
						{/if}
						</div>

					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="lane-items"
						use:dndzone={{
							items: laneData[laneKey(lane)] ?? [],
							flipDurationMs,
							type: 'role-board-card',
							dropTargetClasses: ['drop-target'],
							delayTouchStart: touchDragDelayMs,
							dragDisabled: !canEditAnyItem
						}}
						onconsider={(e) => handleDndConsider(laneKey(lane), e)}
						onfinalize={(e) => handleDndFinalize(laneKey(lane), e)}
						oncontextmenu={(e) => e.preventDefault()}
					>
						{#each (laneData[laneKey(lane)] ?? []) as item (item.id)}
							{@const coll = collectionForItem(item)}
							<div class="card-wrapper" class:dimmed={highlightMine && currentUserId && item.assigned_user_id !== currentUserId}>
								{#if coll}
									<ItemCard {item} collection={coll} compact={true} showCollection={true} />
								{:else}
									<a href="/{username}/{wsSlug}/{item.collection_slug}/{itemUrlId(item)}" class="fallback-card">
										<span class="card-title">{item.title}</span>
									</a>
								{/if}
							</div>
						{/each}
						{#if (laneData[laneKey(lane)] ?? []).length === 0 && !isDragging}
							<div class="lane-empty">No items</div>
						{/if}
					</div>
				</div>
			{/each}

			<!-- Add role column — owner-only (PLAN-1100 / TASK-1108). -->
			{#if isOwner}
				<div class="lane lane-add">
					<button class="add-role-btn" onclick={openCreateModal}>
						<span class="add-role-icon">+</span>
						<span class="add-role-label">Add Role</span>
					</button>
				</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	/* ── Page Layout ──────────────────────────────────────────────────── */
	.role-board-page {
		padding: var(--space-6);
		height: 100%;
		display: flex;
		flex-direction: column;
	}

	/* ── Header ───────────────────────────────────────────────────────── */
	/* Layout-only wrapper around the shared PageHeader (see markup comment). */
	.header-wrap {
		flex-shrink: 0;
	}

	/* ── Toggle Button ────────────────────────────────────────────────── */
	.toggle-btn {
		background: var(--bg-secondary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s, color 0.15s;
	}
	.toggle-btn:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.toggle-btn.active {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
		border-color: var(--accent-blue);
	}

	.new-item-btn {
		background: var(--accent-blue);
		color: white;
		border: none;
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		transition: filter 0.15s;
	}
	.new-item-btn:hover {
		filter: brightness(1.15);
	}

	/* ── New Item Modal ──────────────────────────────────────────────── */
	/* Surface + backdrop + centering are owned by the shared <Modal> primitive
	   (TASK-2023); this modal keeps only its inner content styling. */
	.new-item-content {
		padding: var(--space-5);
		/* The shared <Modal> caps height (max-height: 85vh) with overflow: hidden;
		   scroll the picker content itself so a long collection list / short
		   viewport can't clip the lower buttons. */
		overflow-y: auto;
	}
	.new-item-content .dialog-header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-bottom: var(--space-5);
	}
	.new-item-content .dialog-header h2 {
		flex: 1;
		font-size: 1.05em;
		font-weight: 700;
		margin: 0;
	}
	.back-btn {
		background: none;
		border: none;
		color: var(--text-secondary);
		cursor: pointer;
		font-size: 1.1em;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius);
	}
	.back-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.collection-grid {
		display: grid;
		grid-template-columns: repeat(2, 1fr);
		gap: var(--space-3);
	}
	.collection-pick {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		cursor: pointer;
		transition: border-color 0.15s, background 0.15s;
	}
	.collection-pick:hover {
		border-color: var(--accent-blue);
		background: var(--bg-hover);
	}
	.collection-pick-icon {
		font-size: 1.5em;
	}
	.collection-pick-name {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.new-item-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.new-item-title-input {
		background: var(--bg-primary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3) var(--space-4);
		font-size: 0.95em;
		width: 100%;
	}
	.new-item-title-input:focus {
		outline: none;
		border-color: var(--accent-blue);
	}
	.new-item-create-btn {
		background: var(--accent-blue);
		color: white;
		border: none;
		border-radius: var(--radius);
		padding: var(--space-3) var(--space-5);
		font-size: 0.9em;
		font-weight: 600;
		cursor: pointer;
		transition: filter 0.15s;
	}
	.new-item-create-btn:hover:not(:disabled) {
		filter: brightness(1.15);
	}
	.new-item-create-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	/* ── Lanes Container ──────────────────────────────────────────────── */
	.lanes-container {
		display: flex;
		gap: var(--space-4);
		overflow-x: auto;
		flex: 1;
		align-items: stretch;
		padding-bottom: var(--space-4);
	}

	/* ── Lane ─────────────────────────────────────────────────────────── */
	.lane {
		flex: 0 0 280px;
		min-width: 280px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		display: flex;
		flex-direction: column;
		max-height: 100%;
	}

	.lane.dragging-source {
		opacity: 0.4;
	}
	.lane.drag-over-left {
		box-shadow: -3px 0 0 0 var(--accent-blue);
	}
	.lane-drag-handle {
		cursor: grab;
		color: var(--text-muted);
		font-size: 0.85em;
		user-select: none;
		opacity: 0;
		transition: opacity 0.15s;
	}
	.lane-header:hover .lane-drag-handle {
		opacity: 0.6;
	}
	.lane-header[draggable="true"] {
		cursor: grab;
	}
	.lane-header {
		padding: var(--space-4) var(--space-4) var(--space-3);
		border-bottom: 1px solid var(--border);
		position: sticky;
		top: 0;
		background: var(--bg-secondary);
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		z-index: 1;
	}

	.lane-title-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.lane-icon {
		font-size: 1.1em;
		flex-shrink: 0;
	}
	.lane-name {
		font-weight: 700;
		font-size: 0.95em;
		color: var(--text-primary);
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.unassigned-name {
		color: var(--text-muted);
	}
	.lane-count {
		font-size: 0.75em;
		font-weight: 700;
		background: var(--bg-tertiary);
		color: var(--text-muted);
		padding: 1px 8px;
		border-radius: 10px;
		flex-shrink: 0;
	}

	.lane-tools {
		font-size: 0.75em;
		color: var(--text-muted);
		margin-top: var(--space-1);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* ── Lane Items ───────────────────────────────────────────────────── */
	.lane-items {
		padding: var(--space-2);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		overflow-y: auto;
		flex: 1;
	}

	.lane-items:global(.drop-target) {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}
	.card-wrapper {
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}
	.card-wrapper:active {
		cursor: grabbing;
	}
	.card-wrapper.dimmed {
		opacity: 0.35;
		transition: opacity 0.15s;
	}
	.card-wrapper.dimmed:hover {
		opacity: 0.7;
	}
	.lane-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.85em;
	}

	/* ── Fallback Card ───────────────────────────────────────────────── */
	.fallback-card {
		display: block;
		padding: var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
	}

	/* ── Skeleton ─────────────────────────────────────────────────────── */
	.skeleton-board {
		display: flex;
		gap: var(--space-4);
		flex: 1;
	}
	.skeleton-lane {
		flex: 0 0 280px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.skeleton-lane-header {
		height: 24px;
		width: 60%;
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	.skeleton-card {
		height: 80px;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	@keyframes skeleton-pulse {
		0%,
		100% {
			opacity: 0.5;
		}
		50% {
			opacity: 1;
		}
	}

	/* ── Responsive ───────────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.role-board-page {
			padding: 0;
		}
		.header-wrap {
			padding: var(--space-3) var(--space-4);
		}
		.lanes-container {
			overflow-x: auto;
			scroll-snap-type: x proximity;
			-webkit-overflow-scrolling: touch;
			gap: var(--space-3);
			padding: 0 var(--space-4) var(--space-3);
		}
		.lane {
			min-width: 75vw;
			max-width: 75vw;
			scroll-snap-align: center;
			flex-shrink: 0;
			max-height: none;
		}
	}

	/* ── Lane edit button ─────────────────────────────── */
	.lane-edit-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
		opacity: 0;
		transition: opacity 0.15s;
		margin-left: auto;
	}
	.lane-title-row:hover .lane-edit-btn {
		opacity: 1;
	}
	.lane-edit-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/* ── Add role column ─────────────────────────────── */
	.lane-add {
		background: transparent;
		border: 2px dashed var(--border);
		display: flex;
		align-items: center;
		justify-content: center;
		align-self: flex-start;
		min-height: 0;
		flex: 0 0 auto;
		min-width: auto;
		width: auto;
		padding: var(--space-3);
	}
	.add-role-btn {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-2);
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: var(--space-4);
		border-radius: var(--radius);
		transition: color 0.15s, background 0.15s;
	}
	.add-role-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.add-role-icon {
		font-size: 1.8em;
		font-weight: 300;
		line-height: 1;
	}
	.add-role-label {
		font-size: 0.85em;
		font-weight: 500;
	}

	/* ── Roles Dialog ─────────────────────────────────── */
	/* Surface + backdrop + centering are owned by the shared <Modal> primitive
	   (TASK-2023); this modal keeps only its inner content styling. */
	.dialog-content {
		display: flex;
		flex-direction: column;
		max-height: 80vh;
	}
	.dialog-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}
	.dialog-header h2 {
		margin: 0;
		font-size: 1.1em;
		font-weight: 600;
	}
	.dialog-close {
		background: none;
		border: none;
		font-size: 1.2em;
		color: var(--text-muted);
		cursor: pointer;
		padding: 4px 8px;
		border-radius: var(--radius-sm);
	}
	.dialog-close:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.dialog-body {
		padding: var(--space-4) var(--space-5);
		overflow-y: auto;
	}
	.dialog-footer {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-5);
		border-top: 1px solid var(--border);
	}
	.dialog-footer-right {
		display: flex;
		gap: var(--space-2);
	}

	/* ── Shared form elements ─────────────────────────── */
	.role-edit-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.role-field-group {
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.role-field-label {
		font-size: 0.78em;
		font-weight: 500;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}
	.role-edit-row {
		display: flex;
		gap: var(--space-2);
	}
	.role-input {
		width: 100%;
		padding: 7px 10px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}
	.role-input:focus {
		outline: 2px solid var(--accent-blue);
		outline-offset: -1px;
	}
	.role-btn {
		padding: 5px 12px;
		font-size: 0.82em;
		font-family: inherit;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		cursor: pointer;
	}
	.role-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.role-btn-save {
		background: var(--accent-blue);
		color: white;
		border-color: var(--accent-blue);
	}
	.role-btn-save:hover {
		filter: brightness(1.1);
	}
	.role-btn-danger {
		color: var(--accent-orange);
	}
	.role-btn-danger:hover {
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}
</style>
