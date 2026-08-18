<script lang="ts">
	import { tick, untrack } from 'svelte';
	import type { QuickAction, Item, Collection } from '$lib/types';
	import { parseFields, formatItemRef, parseSettings } from '$lib/types';
	import { api, isConflictOrNotFound } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { collapsePushMessage } from '$lib/push/message';
	import {
		describeDispatch,
		isPrePublishRefusal,
		routePrompt,
		type ClipboardReason,
		type DispatchOutcome,
		type PushPresence
	} from '$lib/push/dispatch';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';

	interface Props {
		actions: QuickAction[];
		item?: Item | null;
		collection: Collection;
		scope: 'item' | 'collection';
		wsSlug: string;
		canEdit?: boolean;
		onmanage?: () => void;
		oncollectionupdated?: (c: Collection) => void;
	}

	let {
		actions,
		item = null,
		collection,
		scope,
		wsSlug,
		canEdit = false,
		onmanage,
		oncollectionupdated
	}: Props = $props();

	let open = $state(false);
	let alignLeft = $state(false);
	let triggerEl = $state<HTMLButtonElement>();

	// ── Inline create-form state ─────────────────────────────────────────
	// `showCreateForm` collapses the action list and swaps in the inline
	// form when the user clicks "+ New quick action" in the footer.
	//
	// The BUG-2281 stopPropagation workaround (open/cancel clicks racing the
	// window click-outside handler on a detached target) is retired: the Menu
	// primitive's outside-click is pointerdown-based and fires BEFORE a row
	// click mutates state, so the detach hazard is structurally gone.
	let showCreateForm = $state(false);
	let labelInputEl: HTMLInputElement | undefined = $state(undefined);

	// The focused "New quick action" row unmounts when the form swaps in —
	// hand focus to the label input so keyboard users aren't dropped on
	// <body> (PR #1022 Codex finding). Reads showCreateForm, writes only
	// DOM focus.
	$effect(() => {
		if (showCreateForm) {
			tick().then(() => labelInputEl?.focus());
		}
	});
	let newLabel = $state('');
	let newPrompt = $state('');
	let newIcon = $state('');
	let saving = $state(false);

	let filtered = $derived(actions.filter((a) => a.scope === scope));

	function resolvePrompt(action: QuickAction): string {
		let prompt = action.prompt;
		const fields = item ? parseFields(item) : {};

		const vars: Record<string, string> = {
			ref: item ? formatItemRef(item) ?? '' : '',
			title: item?.title ?? '',
			status: item ? String(fields['status'] ?? '') : '',
			priority: item ? String(fields['priority'] ?? '') : '',
			collection: collection.name,
			content: item?.content ? item.content.slice(0, 200) : '',
			fields: Object.entries(fields)
				.map(([k, v]) => `${k}: ${v}`)
				.join(', '),
			plan: item ? String(fields['plan'] ?? '') : '',
			phase: item ? String(fields['phase'] ?? fields['plan'] ?? '') : ''
		};

		for (const [key, value] of Object.entries(vars)) {
			prompt = prompt.replaceAll(`{${key}}`, value);
		}

		return prompt;
	}

	// ── Push dispatch (PLAN-2558 S4) ─────────────────────────────────────
	//
	// A quick action used to be a clipboard ferry: resolve the template, copy,
	// let the user paste it into their agent. The templating was always the
	// hard part and it already worked; only the last hop was manual. So the
	// action now PUSHES the resolved prompt to the user's connected agent
	// session, and the clipboard becomes the fallback rather than the
	// mechanism.
	//
	// WHY PRESENCE IS READ WHEN THE MENU OPENS, NOT WHEN A ROW IS CLICKED.
	// Both clipboard APIs — `navigator.clipboard.writeText` and the legacy
	// `execCommand('copy')` — want the user gesture that is still live during
	// the click handler and is gone after a network round-trip (Safari is
	// strictest, but Firefox refuses too). Deciding push-vs-copy from a read
	// issued on the CLICK would therefore put an await in front of the very
	// fallback this slice promises, and the ruling is that nothing silently
	// vanishes. Reading on OPEN keeps the copy synchronous inside the gesture.
	//
	// The cost is a small window: click a row before the first read lands and
	// presence is null, which routes to the clipboard with an honest toast
	// rather than to a push. That is the right way round — a needless paste
	// costs the user one action, a push into nothing loses their instruction —
	// and the window is bounded by how fast a mouse can reach a menu row.

	/** Only item-scope actions can push: the endpoint is
	 *  `POST .../items/{slug}/push`, and a collection-scope action has no item
	 *  to address. Those keep the pre-S4 clipboard behavior exactly. */
	const pushable = $derived(scope === 'item' && !!item);

	/**
	 * Presence re-read cadence while the menu is open. Same 10s as
	 * PushToAgentDialog, and for the same reason: the registry's own worst-case
	 * staleness is ~30s (an ungraceful disconnect is invisible until the next
	 * keepalive write fails), so polling faster would buy precision the data
	 * does not have.
	 */
	const PRESENCE_POLL_MS = 10_000;

	/**
	 * How stale a 'known' answer may get before it degrades to "can't tell".
	 *
	 * A FAILED poll already lands as 'unknown'. A poll that HANGS does not — it
	 * simply never writes, leaving the last count in place indefinitely while
	 * the menu goes on offering a push into a session that may be long gone.
	 * That is the losing direction, so a known answer expires.
	 *
	 * 30s is not arbitrary: it is the server's own worst-case presence staleness
	 * (`watchEventsKeepaliveInterval` — an ungraceful disconnect is invisible
	 * until the next keepalive write fails). Past it an un-refreshed count is no
	 * more informative than no count. Three poll intervals must fail in a row to
	 * reach it. Same bound and same reasoning as PushToAgentDialog.
	 */
	const PRESENCE_MAX_AGE_MS = 30_000;

	/** What we know about who is listening; null until the first read of this
	 *  opening lands. Null is treated as 'unknown' by `routePrompt` — "haven't
	 *  heard" and "couldn't hear" license the same conclusion. */
	let presence = $state<PushPresence | null>(null);
	/** Wall-clock of the last SUCCESSFUL presence read, for the expiry above. */
	let lastAnsweredAt = 0;
	/** True while a push is in flight. The endpoint has NO idempotency key, so
	 *  a second dispatch would deliver the instruction twice. */
	let dispatching = $state(false);

	// Fences for the presence reads. Plain `let`s, never $state: read and
	// written only inside handlers, never in reactive position.
	//  - `presenceGen` identifies one OPENING; a response in flight when the
	//    menu closes must not write into the next one.
	//  - `presenceSeq` identifies one REQUEST; nothing bounds how long one
	//    takes, so a stalled poll can resolve after a later one and overwrite a
	//    fresh count with an older one. Only a strictly newer response applies.
	let presenceGen = 0;
	let presenceSeq = 0;
	let presenceAppliedSeq = 0;

	async function readPresence(gen: number): Promise<void> {
		const seq = ++presenceSeq;
		const stillCurrent = () => gen === presenceGen && seq > presenceAppliedSeq;
		try {
			const resp = await api.sessions.list();
			if (!stillCurrent()) return;
			presenceAppliedSeq = seq;
			lastAnsweredAt = Date.now();
			armExpiry();
			// Count ACCEPTING sessions, not merely connected ones (PLAN-2613
			// S4): only an armed session receives a push, so a quick action
			// that routes to push based on the raw connected count would
			// fire-and-forget into a connected-but-unarmed session and lose the
			// prompt. Filtering to armed here makes push-vs-copy honest — with
			// nothing accepting, the action copies instead. The array (not
			// `count`) is what the server enumerated; the two can only disagree
			// if something is wrong, in which case enumeration is the honest one.
			presence = {
				state: 'known',
				count: resp.sessions?.filter((s) => s.armed).length ?? 0,
				connected: resp.sessions?.length ?? 0
			};
		} catch {
			if (!stillCurrent()) return;
			presenceAppliedSeq = seq;
			// Already the state expiry would produce; nothing left to expire.
			clearTimeout(expiryTimer);
			// Every failure lands here as 'unknown', NEVER as zero — a 503 (no
			// presence registry), a 401 (no resolved user, which is also the
			// answer for a logged-out viewer) and a dead network are all "we
			// can't tell", and rendering them as zero is the exact lie
			// handleListSessions returns 503 rather than an empty list to avoid.
			presence = { state: 'unknown' };
		}
	}

	/** True when a 'known' answer has stopped being refreshed for longer than
	 *  the server's own presence staleness bound. */
	function presenceExpired(): boolean {
		return presence?.state === 'known' && Date.now() - lastAnsweredAt > PRESENCE_MAX_AGE_MS;
	}

	/**
	 * Expire the displayed answer, and retire every read already in flight —
	 * those were issued BEFORE the expiry, so their answers describe the server
	 * as it was back then, and letting one land now would restore the very count
	 * we just declared too old to trust. The next poll carries a newer seq and
	 * still applies.
	 */
	function expirePresence() {
		presence = { state: 'unknown' };
		presenceAppliedSeq = presenceSeq;
	}

	/**
	 * Fire the expiry at the moment it comes due, rather than waiting for the
	 * next poll tick to notice (codex round 3).
	 *
	 * Without this the footer line kept saying "Pushes to your connected agent
	 * session" for up to a full poll interval after the routing had already
	 * switched to the clipboard — the menu contradicting itself, on the one line
	 * whose entire job is to say what the next click will do.
	 */
	function armExpiry() {
		clearTimeout(expiryTimer);
		expiryTimer = setTimeout(expirePresence, PRESENCE_MAX_AGE_MS);
	}
	let expiryTimer: ReturnType<typeof setTimeout> | undefined;

	/**
	 * `presence`, downgraded to 'unknown' if it has gone stale.
	 *
	 * Belt AND braces with `armExpiry`, deliberately: a timer is a request, not
	 * a guarantee. Browsers throttle timers hard in a backgrounded tab, so the
	 * expiry can fire arbitrarily late — long after a user has returned to the
	 * tab and clicked. The timer is what keeps the DISPLAY honest; this check is
	 * what keeps the DECISION correct, and only the decision can lose a message.
	 */
	function currentPresence(): PushPresence | null {
		return presenceExpired() ? { state: 'unknown' } : presence;
	}

	// CONVE-1688: the tracked scope reads only `open` and `pushable`; every
	// $state write is inside `untrack`, so the effect cannot self-invalidate.
	$effect(() => {
		if (!open || !pushable) return;
		const gen = untrack(() => {
			presenceAppliedSeq = 0;
			presence = null;
			lastAnsweredAt = 0;
			return ++presenceGen;
		});
		void readPresence(gen);
		const timer = setInterval(() => {
			// Catches the throttled-timer case: if `expiryTimer` ran late, the
			// answer is already past its bound and must not survive to the next
			// tick just because a timeout was delayed.
			if (presenceExpired()) expirePresence();
			void readPresence(gen);
		}, PRESENCE_POLL_MS);
		return () => {
			clearInterval(timer);
			clearTimeout(expiryTimer);
			// Retire in-flight reads so a late answer can't write into the next
			// opening (or into a closed menu's stale `presence`).
			presenceGen += 1;
		};
	});

	function announce(outcome: DispatchOutcome, copyOffer?: string) {
		const { message, tone } = describeDispatch(outcome);
		// The two push-failure messages carry an instruction ("check your agent
		// session before sending it again") that nobody can read in 3s.
		const longLived = outcome.kind === 'push-refused' || outcome.kind === 'push-unconfirmed';
		toastStore.show(
			message,
			tone,
			longLived ? 9000 : undefined,
			undefined,
			// Offered ONLY where a copy cannot duplicate anything — see the call
			// sites. The click is also a fresh user gesture, which is what makes
			// a clipboard write work this long after the original one.
			//
			// The offer reports its own outcome (codex round 2). Taking it
			// dismisses the toast that carried it, so discarding the result would
			// leave a failed copy completely silent — and this is the one path
			// where that silence means the instruction was neither sent NOR
			// copied, which is the worst state the surface can be in.
			copyOffer
				? {
						label: 'Copy instead',
						onAction: () => void copyAndAnnounce(copyOffer, 'offered')
					}
				: undefined
		);
	}

	async function copyAndAnnounce(text: string, because: ClipboardReason): Promise<void> {
		// The RAW prompt, not the collapsed one: the clipboard has no
		// single-line constraint, so an action whose template spans paragraphs
		// should paste as the author wrote it. Only the push is collapsed, and
		// only because `Notification.Summary` is a one-line wire contract.
		const ok = await copyToClipboard(text);
		announce(ok ? { kind: 'copied', because } : { kind: 'copy-failed', because });
	}

	async function handleAction(action: QuickAction) {
		if (dispatching) return;
		// Capture everything BEFORE any await (BUG-2265 switch-safety): this
		// component is reused across items and collections without a guaranteed
		// remount, so reading a live prop after the await could address the
		// WRONG item.
		const prompt = resolvePrompt(action);
		const ws = wsSlug;
		const target = pushable && item ? item.slug : null;
		// The staleness-aware read, not the raw state: a click can land a whole
		// poll interval after a 'known' answer expired.
		const known = currentPresence();
		const knownCount = known?.state === 'known' ? known.count : 0;
		const route = routePrompt(prompt, target, known);
		open = false;
		resetCreateForm();

		if (route.via === 'clipboard' || !target) {
			// Still inside the click's user gesture — see the note above.
			await copyAndAnnounce(prompt, route.via === 'clipboard' ? route.because : 'not-addressable');
			return;
		}

		dispatching = true;
		try {
			// NEVER retried automatically, here or anywhere else: the endpoint
			// carries no idempotency key.
			await api.items.push(ws, target, collapsePushMessage(prompt));
			announce({ kind: 'pushed', count: knownCount });
		} catch (err) {
			if (isPrePublishRefusal(err)) {
				// The server refused before publishing, so nothing went out and
				// handing the text over cannot deliver it twice — offer the copy.
				announce(
					{ kind: 'push-refused', detail: err instanceof Error ? err.message : '' },
					prompt
				);
			} else {
				// Outcome genuinely unknown: the handler publishes BEFORE it
				// writes its response, so the instruction may already have
				// landed. No copy offer here — a paste would be the duplicate the
				// message is warning about.
				announce({ kind: 'push-unconfirmed' });
			}
		} finally {
			dispatching = false;
		}
	}

	/** The menu footer line. Says what the NEXT click will do, so the routing
	 *  is visible before it happens rather than only in the toast after. */
	const tagline = $derived.by(() => {
		if (!pushable) return 'Copy a prompt to your agent';
		if (!presence) return 'Checking for connected agent sessions…';
		if (presence.state === 'unknown') {
			return 'Can’t tell if an agent is connected — actions copy to your clipboard';
		}
		// Honest split (PLAN-2613 S4, D3): `count` is the ACCEPTING subset that
		// decides push-vs-copy; `connected` is the total, shown so a
		// connected-but-unarmed session isn't hidden behind a bare zero.
		const accepting = presence.count;
		const connected = presence.connected ?? accepting;
		if (accepting === 0) {
			return connected > 0
				? `${connected} connected, 0 accepting pushes — actions copy; run /pad:connect to enable`
				: 'No agent session connected — actions copy to your clipboard';
		}
		const ofConnected = connected > accepting ? ` (of ${connected} connected)` : '';
		return accepting === 1
			? `Pushes to 1 accepting session${ofConnected}`
			: `Pushes to ${accepting} accepting sessions${ofConnected}`;
	});

	function resetCreateForm() {
		showCreateForm = false;
		newLabel = '';
		newPrompt = '';
		newIcon = '';
	}

	function handleManage() {
		// Recheck `canEdit` at dispatch time (mirrors handleSaveNewAction): if it
		// flips false while the menu is open — e.g. the master-freeze passing
		// canEdit=false once this side becomes the peeking preview (BUG-2263) — refuse
		// to open the collection-settings editor. The trigger itself unmounts on the
		// same flip (`{#if canEdit}`); this guards the render→click race.
		if (!canEdit) return;
		open = false;
		resetCreateForm();
		onmanage?.();
	}

	async function handleSaveNewAction() {
		// Recheck `canEdit` at dispatch time: if it flips false while the create
		// form is open (e.g. the PLAN-2154 master-freeze passing canEdit=false
		// while peeking, TASK-2172), refuse the api.collections.update. The form
		// itself unmounts on the same flip (see `{#if showCreateForm && canEdit}`).
		if (!canEdit) return;
		const label = newLabel.trim();
		const prompt = newPrompt.trim();
		if (!label || !prompt || saving) return;
		saving = true;
		// Capture workspace + collection identity BEFORE any await. This
		// component is reused across items/collections without a guaranteed
		// remount, so reading live props after the await could fetch/update the
		// WRONG collection on a mid-save navigation (BUG-2265 switch-safety).
		const ws = wsSlug;
		const baseCollection = collection;
		const slug = baseCollection.slug;
		try {
			const icon = newIcon.trim();
			const newAction: QuickAction = {
				label,
				prompt,
				scope,
				...(icon ? { icon } : {})
			};

			// BUG-2265: append onto a base snapshot AND round-trip its
			// updated_at so the server rejects a stale write instead of
			// clobbering a sibling ItemDetail's concurrent settings change.
			// `appendOnto` re-derives the full settings from a FRESH base
			// each time, so a 409-triggered refetch+retry re-applies our new
			// action onto whatever the other writer just saved (no silent
			// loss) rather than replaying our stale local snapshot.
			const appendOnto = (base: Collection): string => {
				const s = parseSettings(base);
				return JSON.stringify({
					...s,
					quick_actions: [...(s.quick_actions ?? []), newAction]
				});
			};

			let updated: Collection;
			try {
				updated = await api.collections.update(ws, slug, {
					settings: appendOnto(baseCollection),
					expected_updated_at: baseCollection.updated_at
				});
			} catch (err) {
				// A concurrent change can defeat our slug-targeted write two ways:
				// a 409 update_conflict (settings changed) OR a 404 not_found (a
				// RENAME killed the slug). Recover from BOTH (BUG-2265 Pattern C):
				// resolve the collection by its STABLE id, re-append onto its
				// fresh settings, and retry ONCE. Surface only if it's truly gone.
				if (!isConflictOrNotFound(err)) throw err;
				const list = await api.collections.list(ws);
				const fresh = list.find((c) => c.id === baseCollection.id);
				if (!fresh) throw err; // collection gone (deleted) — surface it
				updated = await api.collections.update(ws, fresh.slug, {
					settings: appendOnto(fresh),
					expected_updated_at: fresh.updated_at
				});
			}
			// Only propagate the result if this component still represents the
			// SAME collection it started with — compared by STABLE id so a
			// concurrent rename (slug changed, same collection) still
			// propagates, while a genuine switch to a DIFFERENT collection
			// (different id) is dropped. On a reused route (no guaranteed
			// remount) `oncollectionupdated` is the live (navigated) page's
			// callback — feeding it our old response would assign stale data to
			// the wrong page (Codex switch-safety).
			if (wsSlug !== ws || collection?.id !== baseCollection.id) return;
			toastStore.show('Saved', 'success');
			oncollectionupdated?.(updated);
			resetCreateForm();
			open = false;
		} catch (err) {
			toastStore.show(
				err instanceof Error ? err.message : 'Failed to save quick action',
				'error'
			);
		} finally {
			saving = false;
		}
	}

	function handleTriggerClick() {
		const nextOpen = !open;
		if (nextOpen && triggerEl) {
			const rect = triggerEl.getBoundingClientRect();
			// If the trigger is too close to the left edge, the default
			// right-anchored dropdown would clip — switch to left-anchored.
			// (Only matters for the desktop anchored panel; the mobile
			// BottomSheet ignores alignment, so computing it is harmless.)
			alignLeft = rect.left < 220;
		}
		if (!nextOpen) {
			// Closing via trigger toggles the form off too so the next open
			// returns to the action list.
			resetCreateForm();
		}
		open = nextOpen;
	}

	function closeMenu() {
		open = false;
		resetCreateForm();
	}

	// The EmojiPickerButton portals its dropdown to document.body (or the
	// nearest <dialog>), so it is NOT inside the Menu panel's DOM — pass its
	// containers as outside-click exemptions or the in-progress emoji
	// selection closes the whole menu before the bound value can update.
	// Queried from document because of the portal.
	function emojiPickerContainers(): (Element | null | undefined)[] {
		return [
			...document.querySelectorAll('.epb-dropdown'),
			...document.querySelectorAll('.emoji-picker-button')
		];
	}
</script>

{#snippet createForm()}
	<div class="create-form">
		<div class="qa-row">
			<EmojiPickerButton bind:value={newIcon} placeholder="+" size="sm" />
			<input
				class="qa-label-input"
				type="text"
				placeholder="Action label"
				bind:value={newLabel}
				bind:this={labelInputEl}
			/>
		</div>
		<textarea
			class="qa-prompt-input"
			placeholder="/pad ..."
			rows="3"
			bind:value={newPrompt}
		></textarea>
		<div class="qa-help">
			Template variables: {'{ref}'} {'{title}'} {'{status}'} {'{priority}'} {'{collection}'} {'{content}'} {'{fields}'}
			{#if scope === 'item'}
				<br />
				Pushes to your agent session as a single line when one is accepting pushes;
				copies to your clipboard otherwise.
			{/if}
		</div>
		<div class="qa-actions">
			<button class="qa-btn qa-btn-cancel" type="button" onclick={resetCreateForm}>
				Cancel
			</button>
			<button
				class="qa-btn qa-btn-save"
				type="button"
				onclick={handleSaveNewAction}
				disabled={!newLabel.trim() || !newPrompt.trim() || saving}
			>
				{saving ? 'Saving...' : 'Save'}
			</button>
		</div>
	</div>
{/snippet}

{#snippet actionList()}
	{#if showCreateForm && canEdit}
		{@render createForm()}
	{:else}
		{#each filtered as action (action.label)}
			<MenuItem icon={action.icon} onclick={() => handleAction(action)}>
				{action.label}
			</MenuItem>
		{/each}
		<!-- Plain div, deliberately not role="status": the panel is role="menu",
		     whose children must be menuitem/group/separator, and a live region
		     nested in a menu is unreliably announced anyway. The routing reaches
		     assistive tech through the menu's own accessible name instead (see
		     `ariaLabel` below), which is read when focus enters the panel. -->
		<div class="dropdown-tagline">{tagline}</div>
		{#if canEdit}
			<div class="footer-divider"></div>
			<MenuItem icon="+" onclick={() => (showCreateForm = true)}>New quick action</MenuItem>
			<MenuItem icon="⚙" onclick={handleManage}>Manage actions</MenuItem>
		{/if}
	{/if}
{/snippet}

{#if filtered.length > 0 || canEdit}
	<div class="quick-actions-menu">
		<button
			bind:this={triggerEl}
			class="trigger-btn"
			aria-haspopup="menu"
			aria-expanded={open}
			onclick={handleTriggerClick}
			title="Quick actions"
		>
			&#9889;
		</button>

		<!-- mode=portal (BUG-2610): the split-view pane is an
		     overflow-y:auto scroll container, which computes
		     overflow-x:auto too — an anchored right-aligned panel opening
		     from the pane's action bar extended past the pane's left edge
		     and was CLIPPED mid-text. Portal escapes overflow containment
		     and viewport-clamps/flips when cramped. width covers the
		     .qa-body min-width (230) plus panel chrome. -->
		<Menu
			{open}
			onclose={closeMenu}
			trigger={triggerEl}
			align={alignLeft ? 'left' : 'right'}
			mode="portal"
			width={288}
			sheetOnMobile
			sheetTitle="Quick actions"
			ariaLabel={pushable ? `Quick actions. ${tagline}` : 'Quick actions'}
			exempt={emojiPickerContainers}
		>
			<div class="qa-body">
				{@render actionList()}
			</div>
		</Menu>
	</div>
{/if}

<style>
	.quick-actions-menu {
		/* Anchor for Menu's anchored mode. */
		position: relative;
		display: inline-block;
	}

	.trigger-btn {
		padding: 2px var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
	}

	.trigger-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	/* Sizes the slotted content (Menu's panel is min-width 180px on its
	   own — too narrow for the inline create form). The viewport cap is
	   defensive: the popover should never overflow horizontally even if
	   trigger placement or zoom produces an edge case we didn't anticipate. */
	.qa-body {
		min-width: 230px;
		max-width: calc(100vw - var(--space-4));
	}

	.dropdown-tagline {
		padding: var(--space-2) var(--space-3);
		font-size: 0.72em;
		color: var(--text-muted);
		border-top: 1px solid var(--border);
		text-align: center;
	}

	/* ── Footer (gated by canEdit) ──────────────────────────────────── */

	.footer-divider {
		height: 1px;
		background: var(--border);
		margin: var(--space-1) 0;
	}

	/* ── Inline create form ─────────────────────────────────────────── */

	.create-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
	}

	.qa-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.qa-label-input {
		flex: 1;
		min-width: 0;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.qa-label-input:hover {
		border-color: var(--border);
	}

	.qa-label-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.qa-prompt-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-primary);
		resize: vertical;
		min-height: 60px;
	}

	.qa-prompt-input:hover {
		border-color: var(--border);
	}

	.qa-prompt-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.qa-help {
		font-size: 0.7em;
		color: var(--text-muted);
		line-height: 1.4;
		word-break: break-word;
	}

	.qa-actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-top: var(--space-1);
	}

	.qa-btn {
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.8em;
		cursor: pointer;
		border: 1px solid var(--border);
	}

	.qa-btn-cancel {
		background: var(--bg-tertiary);
		color: var(--text-secondary);
	}

	.qa-btn-cancel:hover {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}

	.qa-btn-save {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
		font-weight: 500;
	}

	.qa-btn-save:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.qa-btn-save:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
