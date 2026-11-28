<script lang="ts">
	// One agent on the Home Agents panel (DOC-4851 section 4): a collapsed
	// strip, and when expanded one card per lane with Now, Up next, the park
	// line and Just finished. Every write is gated by what the server said the
	// caller may do (can_reorder, can_edit, can_park); the rest is read-only.
	//
	// Display = server lane + the store's optimistic overlay. Only the handlers
	// below and a published load write the overlay; the dragged token is a
	// plain let, never state an effect reads.
	//
	// One write per lane at a time: while the store marks a lane pending (a
	// write in flight, or landed and not yet shown by a reload) its handles,
	// drops and row actions are off, so no write is planned on stale values.
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { AGENTS_SHOW_ALL_LIMIT, agentsOverview, laneKey, type LaneOverlay } from '$lib/stores/agentsOverview.svelte';
	import { avatarColor } from '$lib/utils/avatar';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { LINE, age, boundLane, doNextOrder, fromTokens, itemHref, laneView, lineLabel, lineRequest, moveToken, parkedLabel, planLane, stepOrder, tokens, type LaneView } from './agentLane';
	import type { AgentRole, MeAgent, RoleParkRequest, RoleQueueItem, RoleQueueResponse } from '$lib/types';
	import { shouldOpenInPane } from '$lib/components/collections/itemCardClick';
	import { homePaneKey, type HomePaneTarget } from './homePane';

	interface Props {
		agent: MeAgent;
		zone: string;
		compact?: boolean;
		expanded: boolean;
		ontoggle: () => void;
		/** Hold live refreshes while a drag or write is in flight; the release refetches. */
		hold: () => (refresh?: boolean) => void;
		/** Open a ref in the home item pane (desktop, TASK-4902); unset, refs navigate. */
		onopenitem?: (target: HomePaneTarget) => void;
	}
	let { agent, zone, compact = false, expanded, ontoggle, hold, onopenitem }: Props = $props();

	// A plain primary click opens the pane; modifier and middle clicks, and the
	// phone layout, keep the link's own navigation.
	function openRef(e: MouseEvent, target: HomePaneTarget) {
		if (compact || !onopenitem || !shouldOpenInPane(e, true)) return;
		e.preventDefault();
		onopenitem(target);
	}

	const lanes = $derived(agent.lanes);
	const nowItems = $derived(lanes.flatMap(lane => lane.now.map(item => ({ lane, item }))));
	const first = $derived(nowItems[0]);
	const parked = $derived(parkedLabel(lanes));
	const canParkAny = $derived(lanes.some(lane => lane.can_park));
	const canUnparkAny = $derived(lanes.some(lane => lane.can_park && lane.park));
	const queued = $derived(agent.summary.queue_total);
	const queuedText = $derived(queued ? `${queued} queued` : 'Queue empty');
	const wsName = (slug: string) => workspaceStore.workspaces.find(ws => ws.slug === slug)?.name ?? slug;
	const views = $derived(new Map(lanes.map(lane => [laneKey(lane), laneView(lane, agentsOverview.overlay[laneKey(lane)])])));
	const viewOf = (lane: RoleQueueResponse): LaneView => views.get(laneKey(lane)) ?? laneView(lane);
	const locked = (lane: RoleQueueResponse) => agentsOverview.isPending(laneKey(lane));
	const anyLocked = $derived(lanes.some(lane => agentsOverview.isPending(laneKey(lane))));
	const extraNow = $derived.by(() => {
		const rest = nowItems.length - 1;
		if (rest < 1 || !first) return '';
		const other = nowItems.slice(1).find(n => n.lane.workspace !== first.lane.workspace);
		return rest === 1 && other ? `+1 in ${wsName(other.lane.workspace)}` : `+${rest} more`;
	});
	const liveText = { busy: 'Live: busy', idle: 'Live: idle', offline: 'Live: offline', unknown: '' } as const;

	// Phone: one lane open at a time.
	let openLane = $state('');
	const laneOpen = (lane: RoleQueueResponse, index: number) => !compact || (openLane ? openLane === laneKey(lane) : index === 0);

	let menuKey = $state('');
	let menuTrigger = $state<HTMLElement | undefined>(undefined);
	let reassigning = $state<{ key: string; roles: AgentRole[] | null } | null>(null);
	let busy = $state(false);

	const fail = (message: string) => toastStore.show(message, 'error');
	const codeOf = (err: unknown) => (err as { code?: string } | null)?.code ?? '';

	// ── Writes ──────────────────────────────────────────────────────────────

	async function reorder(lane: RoleQueueResponse, next: LaneOverlay, movedRef: string, notice?: string) {
		const key = laneKey(lane);
		if (locked(lane)) return;
		const shown = lane.queue.length;
		// A truncated lane's end is not open: at the queue read limit there is no
		// next item to bound it, and a whole-lane plan (tied values, a shift or a
		// respace) can write past the unshown items wherever the mover lands, so
		// reordering such a lane is left to the Roles board, which loads it whole.
		if (lane.queue_more && shown >= AGENTS_SHOW_ALL_LIMIT) {
			fail('This queue is longer than shown here. Use the Roles board to reorder it.');
			return;
		}
		const sameIdentity = authStore.identityFence();
		const release = hold();
		agentsOverview.beginWrite(key);
		agentsOverview.setOverlay(key, next);
		// ok: the lane stays pending until a reload shows the server state.
		let ok = false;
		try {
			let planned = lane;
			let bound: RoleQueueItem | null = null;
			if (lane.queue_more && shown < AGENTS_SHOW_ALL_LIMIT) {
				const fresh = boundLane(lane, await api.agentRoles.queue(lane.workspace, lane.role.slug, shown + 1));
				if (!sameIdentity()) return;
				if (!fresh.ok) {
					agentsOverview.clearOverlay(key);
					toastStore.show('The queue changed meanwhile. Showing the latest.', 'info');
					ok = true;
					return;
				}
				planned = fresh.lane;
				bound = fresh.bound;
			}
			let lineSort: number | null = null;
			if (planned.park?.mode === 'line') lineSort = planned.park.sort_order ?? null;
			const plan = planLane(planned, next, movedRef, lineSort, bound);
			if (!plan.ok) {
				agentsOverview.clearOverlay(key);
				fail("Couldn't keep that order: an item you can only view is in the way.");
				return;
			}
			if (plan.writes.length) {
				const idByRef = new Map(planned.queue.map(item => [item.ref, item.id]));
				await api.agentRoles.reorder(lane.workspace, plan.writes.map(w => ({ item_id: idByRef.get(w.id) ?? w.id, role_sort_order: w.sort_order })));
				if (!sameIdentity()) return;
			}
			ok = true;
			if (notice) toastStore.show(notice, 'info');
		} catch {
			if (!sameIdentity()) return;
			agentsOverview.clearOverlay(key);
			fail("Couldn't save the queue order.");
		} finally {
			if (sameIdentity()) {
				agentsOverview.endWrite(key, ok);
				release(true);
			}
		}
	}

	async function park(lane: RoleQueueResponse, body: RoleParkRequest, next?: LaneOverlay) {
		const key = laneKey(lane);
		if (locked(lane)) return;
		const sameIdentity = authStore.identityFence();
		const release = hold();
		agentsOverview.beginWrite(key);
		if (next) agentsOverview.setOverlay(key, next);
		let ok = false;
		try {
			await api.agentRoles.park(lane.workspace, lane.role.slug, lane.park ? { ...body, expected_park_seq: lane.park.park_seq } : body);
			ok = true;
		} catch (err) {
			if (!sameIdentity()) return;
			agentsOverview.clearOverlay(key);
			fail(codeOf(err) === 'park_conflict' ? 'The stop line moved meanwhile. Showing the latest.' : "Couldn't move the stop line.");
		} finally {
			if (sameIdentity()) {
				agentsOverview.endWrite(key, ok);
				release(true);
			}
		}
	}

	async function unpark(lane: RoleQueueResponse) {
		const key = laneKey(lane);
		if (locked(lane)) return;
		const sameIdentity = authStore.identityFence();
		const release = hold();
		agentsOverview.beginWrite(key);
		let ok = false;
		try {
			await api.agentRoles.unpark(lane.workspace, lane.role.slug, lane.park?.park_seq);
			ok = true;
		} catch (err) {
			if (sameIdentity()) fail(codeOf(err) === 'park_conflict' ? 'The stop line moved meanwhile. Showing the latest.' : "Couldn't unpark this lane.");
		} finally {
			if (sameIdentity()) {
				agentsOverview.endWrite(key, ok);
				release(true);
			}
		}
	}

	async function parkAfterCurrent() {
		if (busy || anyLocked) return;
		const sameIdentity = authStore.identityFence();
		const release = hold();
		busy = true;
		const target = lanes.filter(lane => lane.can_park);
		const skipped = lanes.filter(lane => !lane.can_park).map(lane => wsName(lane.workspace));
		for (const lane of target) agentsOverview.beginWrite(laneKey(lane));
		const results = await Promise.allSettled(target.map(lane => api.agentRoles.park(lane.workspace, lane.role.slug, lane.park ? { at: 'top', expected_park_seq: lane.park.park_seq } : { at: 'top' })));
		if (!sameIdentity()) return;
		busy = false;
		target.forEach((lane, i) => agentsOverview.endWrite(laneKey(lane), results[i].status === 'fulfilled'));
		const failed = target.filter((_, i) => results[i].status === 'rejected').map(lane => wsName(lane.workspace));
		const done = target.length - failed.length;
		const parts = [`${agent.name} parked after current work on ${done} ${done === 1 ? 'lane' : 'lanes'}.`];
		if (skipped.length) parts.push(`Skipped (view only): ${skipped.join(', ')}.`);
		if (failed.length) parts.push(`Failed: ${failed.join(', ')}.`);
		toastStore.show(parts.join(' '), failed.length ? 'error' : 'success');
		release(true);
	}

	async function unparkAll() {
		if (busy || anyLocked) return;
		const sameIdentity = authStore.identityFence();
		const release = hold();
		busy = true;
		const target = lanes.filter(lane => lane.can_park && lane.park);
		for (const lane of target) agentsOverview.beginWrite(laneKey(lane));
		const results = await Promise.allSettled(target.map(lane => api.agentRoles.unpark(lane.workspace, lane.role.slug, lane.park?.park_seq)));
		if (!sameIdentity()) return;
		busy = false;
		target.forEach((lane, i) => agentsOverview.endWrite(laneKey(lane), results[i].status === 'fulfilled'));
		if (results.some(r => r.status === 'rejected')) fail(`Couldn't unpark every lane of ${agent.name}.`);
		release(true);
	}

	function doNext(lane: RoleQueueResponse, ref: string) {
		const view = viewOf(lane);
		const next = doNextOrder(view, ref);
		void reorder(lane, next, ref, view.mode === 'top' ? `Queued first; ${lane.role.name} is parked` : undefined);
	}

	function step(lane: RoleQueueResponse, ref: string, delta: -1 | 1) {
		const next = stepOrder(viewOf(lane), ref, delta);
		if (next) void reorder(lane, next, ref);
	}

	async function openReassign(lane: RoleQueueResponse, ref: string) {
		const key = `${laneKey(lane)}#${ref}`;
		const sameIdentity = authStore.identityFence();
		reassigning = { key, roles: null };
		try {
			const roles = await api.agentRoles.list(lane.workspace);
			if (sameIdentity() && reassigning?.key === key) reassigning = { key, roles: roles.filter(role => role.slug !== lane.role.slug) };
		} catch {
			if (sameIdentity() && reassigning?.key === key) { reassigning = null; menuKey = ''; fail("Couldn't load this workspace's roles."); }
		}
	}

	async function reassign(lane: RoleQueueResponse, ref: string, role: AgentRole) {
		closeMenu();
		const key = laneKey(lane);
		if (locked(lane)) return;
		const sameIdentity = authStore.identityFence();
		const release = hold();
		agentsOverview.beginWrite(key);
		let ok = false;
		try {
			// The Roles board's cross-lane move; the server lands it at the bottom of the target lane.
			await api.items.update(lane.workspace, ref, { agent_role_id: role.id });
			ok = true;
			if (sameIdentity()) toastStore.show(`${ref} moved to ${role.name}.`, 'success');
		} catch {
			if (sameIdentity()) fail(`Couldn't move ${ref} to ${role.name}.`);
		} finally {
			if (sameIdentity()) {
				agentsOverview.endWrite(key, ok);
				release(true);
			}
		}
	}

	function closeMenu() {
		menuKey = '';
		reassigning = null;
	}

	// ── Drag (desktop only) ─────────────────────────────────────────────────

	let dragLane = '';
	let dragToken = '';
	let dragRelease: ((refresh?: boolean) => void) | null = null;
	let dropAt = $state('');

	function dragStart(e: DragEvent, lane: RoleQueueResponse, token: string) {
		dragLane = laneKey(lane);
		dragToken = token;
		dragRelease = hold();
		e.dataTransfer?.setData('text/plain', token === LINE ? 'stop line' : token);
		if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move';
	}

	function dragEnd() {
		dragLane = '';
		dragToken = '';
		dropAt = '';
		const r = dragRelease;
		dragRelease = null;
		r?.();
	}

	/** Whether this lane takes the dragged token now: the line where can_park, an item where it may move. */
	function accepts(lane: RoleQueueResponse, token: string) {
		if (locked(lane)) return false;
		if (token === LINE) return lane.can_park;
		const item = lane.queue.find(i => i.ref === token);
		return !!item && canMove(lane, item);
	}

	function dragOver(e: DragEvent, lane: RoleQueueResponse, target: string) {
		if (!dragToken || dragLane !== laneKey(lane) || !accepts(lane, dragToken)) return;
		e.preventDefault();
		dropAt = `${laneKey(lane)}#${target}`;
	}

	/** Drop the dragged token before `target` (an item ref, LINE, or '' for the end). */
	function drop(e: DragEvent, lane: RoleQueueResponse, target: string) {
		if (!dragToken || dragLane !== laneKey(lane) || !accepts(lane, dragToken)) return;
		e.preventDefault();
		const token = dragToken;
		const view = viewOf(lane);
		const list = tokens(view);
		const from = list.indexOf(token);
		let to = target ? list.indexOf(target) : list.length;
		if (from < to) to--;
		if (view.mode === 'top' && token !== LINE) to = Math.max(1, to);
		if (from >= 0 && to >= 0 && from !== to) {
			const next = fromTokens(moveToken(list, from, to));
			// The write takes its own hold first, so releasing the drag's hold
			// does not let a deferred refresh overtake it.
			if (token === LINE) {
				const body = next.line === null ? null : lineRequest(next.refs, next.line);
				if (body) void park(lane, body, next);
			} else {
				void reorder(lane, next, token);
			}
		}
		dragEnd();
	}

	const canMove = (lane: RoleQueueResponse, item: RoleQueueItem) => lane.can_reorder && item.can_edit;
	const priorityMark: Record<string, string> = { critical: '▲▲', high: '▲', medium: '■', low: '▽' };
</script>

{#snippet refLink(owner: string, workspace: string, collection: string, ref: string)}
	{@const href = itemHref(owner, workspace, collection, ref)}
	{#if href}<a class="ref" {href} draggable="false" data-pane-key={homePaneKey(owner, workspace, ref)} onclick={(e) => openRef(e, { owner, workspace, collection, ref })}>{ref}</a>{:else}<span class="ref">{ref}</span>{/if}
{/snippet}

{#snippet lineRule(lane: RoleQueueResponse, view: LaneView)}
	{@const draggable = !compact && lane.can_park && !locked(lane)}
	<li class="line" class:drop={dropAt === `${laneKey(lane)}#${LINE}`} role="separator" aria-label={`${lineLabel(lane.park?.set_by ?? null, lane.park?.set_at ?? null, zone)}${view.mode === 'top' ? ', before every queued item' : ''}`}
		draggable={draggable ? 'true' : undefined}
		ondragstart={draggable ? (e) => dragStart(e, lane, LINE) : undefined}
		ondragend={draggable ? dragEnd : undefined}
		ondragover={(e) => dragOver(e, lane, LINE)}
		ondrop={(e) => drop(e, lane, LINE)}>
		<span class="rule" aria-hidden="true"></span>
		<span class="stop" class:grab={draggable}>{#if !compact && lane.can_park}<span class="handle" aria-hidden="true">⠿</span>{/if}<strong>Stop here.</strong> {lineLabel(lane.park?.set_by ?? null, lane.park?.set_at ?? null, zone).replace(/^Stop here\.? ?/, '')}
			{#if lane.can_park}<button type="button" class="unpark-x" aria-label={`Unpark ${lane.role.name} in ${wsName(lane.workspace)}`} disabled={locked(lane)} onclick={() => unpark(lane)}>×</button>{/if}
		</span>
		<span class="rule" aria-hidden="true"></span>
	</li>
{/snippet}

<li class="agent" class:open={expanded}>
	<button type="button" class="strip" aria-expanded={expanded} data-agent-toggle={agent.slug} onclick={ontoggle}>
		<span class="caret" aria-hidden="true">{expanded ? '▾' : '▸'}</span>
		<span class="icon" aria-hidden="true">{agent.icon || '🤖'}</span>
		<span class="name">{agent.name}</span>
		<span class="now">
			{#if first}
				<span class="label">Now</span> <span class="ref">{first.item.ref}</span> <span class="title">{first.item.title}</span> <span class="ws">{wsName(first.lane.workspace)}</span>{#if extraNow} <span class="extra">{extraNow}</span>{/if}
			{:else}
				<em class="idle">Idle</em>
			{/if}
		</span>
		<span class="age" title={first ? `Since ${first.item.since}` : undefined}>{first ? age(first.item.since) : ''}</span>
		{#if first && first.item.live !== 'unknown'}<span class={`dot ${first.item.live}`} role="img" aria-label={liveText[first.item.live]}></span>{:else}<span class="dot none" aria-hidden="true"></span>{/if}
		<span class="queued">{queuedText}</span>
		<span class="parked-slot">{#if parked}<span class="chip parked" class:some={parked !== 'parked'}>❙❙ {parked}</span>{/if}</span>
	</button>

	{#if expanded}
		<div class="detail">
			{#if canParkAny || canUnparkAny}
				<div class="actions">
					{#if canParkAny}<button type="button" class="action" disabled={busy || anyLocked} onclick={parkAfterCurrent}>❙❙ Park after current</button>{/if}
					{#if canUnparkAny}<button type="button" class="action" disabled={busy || anyLocked} onclick={unparkAll}>▶ Unpark</button>{/if}
					<span class="lanes-count">{lanes.length} {lanes.length === 1 ? 'lane' : 'lanes'}</span>
				</div>
			{/if}
			<div class="lanes" class:single={lanes.length === 1}>
				{#each lanes as lane, laneIndex (laneKey(lane))}
					{@const view = viewOf(lane)}
					{@const key = laneKey(lane)}
					{@const finished = agent.finished.filter(f => f.workspace === lane.workspace)}
					<section class="lane" aria-label={`${agent.name} in ${wsName(lane.workspace)}`}>
						{#if compact}
							<button type="button" class="lane-head" aria-expanded={laneOpen(lane, laneIndex)} onclick={() => (openLane = key)}>
								<span class="avatar" style:background={avatarColor(wsName(lane.workspace))}>{wsName(lane.workspace).charAt(0).toUpperCase()}</span><strong>{wsName(lane.workspace)}</strong>
								{#if !laneOpen(lane, laneIndex)}<span class="lane-sum">{lane.now[0] ? `Now ${lane.now[0].ref} · ` : ''}{lane.queue_total} queued</span>{/if}
								{#if lane.park}<span class="chip parked">❙❙ {lane.park.mode === 'top' ? 'parked' : `parked after ${view.line ?? lane.queue_total}`}</span>{/if}
							</button>
						{:else}
							<header class="lane-head">
								<span class="avatar" style:background={avatarColor(wsName(lane.workspace))}>{wsName(lane.workspace).charAt(0).toUpperCase()}</span><strong>{wsName(lane.workspace)}</strong>
								{#if lane.park}<span class="chip parked">❙❙ {lane.park.mode === 'top' ? 'parked' : `parked after ${view.line ?? lane.queue_total}`}</span>{:else if !lane.can_reorder && !lane.can_park}<span class="chip view">view only</span>{/if}
							</header>
						{/if}
						{#if laneOpen(lane, laneIndex)}
							{#if lane.now.length}
								<h4>Now</h4>
								<ul>
									{#each lane.now.slice(0, 3) as item (item.ref)}
										<li class="row now-row">
											{#if item.live !== 'unknown'}<span class={`dot ${item.live}`} role="img" aria-label={liveText[item.live]}></span>{/if}
											{@render refLink(lane.owner_username, lane.workspace, item.collection, item.ref)}<span class="title strong">{item.title}</span>
											{#if item.status}<span class="chip status">{item.status}</span>{/if}<span class="age">{age(item.since)}</span>
										</li>
									{/each}
								</ul>
								{#if lane.now.length > 3}<p class="more">+{lane.now.length - 3} more in progress</p>{/if}
							{/if}
							<h4>Up next · {lane.queue_total}</h4>
							<ul class="queue">
								{#if view.line === 0}{@render lineRule(lane, view)}{/if}
								{#each view.items as item, i (item.ref)}
									{@const lk = locked(lane)}
									{@const movable = !compact && canMove(lane, item) && !lk}
									{@const isParked = view.line !== null && i >= view.line}
									{@const mk = `${key}#${item.ref}`}
									<li class="row" class:parked={isParked} class:drop={dropAt === mk}
										draggable={movable ? 'true' : undefined}
										ondragstart={movable ? (e) => dragStart(e, lane, item.ref) : undefined}
										ondragend={movable ? dragEnd : undefined}
										ondragover={(e) => dragOver(e, lane, item.ref)}
										ondrop={(e) => drop(e, lane, item.ref)}>
										<span class="handle" aria-hidden="true">{!compact && canMove(lane, item) ? '⠿' : ''}</span>
										<span class="pos">{i + 1}</span>{@render refLink(lane.owner_username, lane.workspace, item.collection, item.ref)}<span class="title">{item.title}</span>
										{#if item.blocked}<span class="chip blocked">blocked</span>{/if}
										{#if isParked}<em class="parked-tag">parked</em>{/if}
										{#if item.priority}<span class={`prio ${item.priority}`} title={item.priority}><span aria-hidden="true">{priorityMark[item.priority] ?? ''}</span> <span class="prio-word">{item.priority.charAt(0).toUpperCase() + item.priority.slice(1)}</span></span>{/if}
										{#if lane.can_reorder && item.can_edit || lane.can_park}
											<button type="button" class="more-btn" aria-label={`Actions for ${item.ref}`} aria-haspopup="menu" aria-expanded={menuKey === mk}
												onclick={(e) => { menuTrigger = e.currentTarget; reassigning = null; menuKey = menuKey === mk ? '' : mk; }}>…</button>
											<Menu open={menuKey === mk} trigger={menuTrigger} mode="portal" ariaLabel={`Actions for ${item.ref}`} onclose={closeMenu} focusKey={reassigning?.key === mk ? 'reassign' : 'actions'}>
												{#if reassigning?.key === mk}
													{#if reassigning.roles === null}<MenuItem disabled>Loading roles…</MenuItem>
													{:else if reassigning.roles.length === 0}<MenuItem disabled>No other roles here</MenuItem>
													{:else}{#each reassigning.roles as role (role.id)}<MenuItem icon={role.icon || undefined} onclick={() => reassign(lane, item.ref, role)}>{role.name}</MenuItem>{/each}{/if}
												{:else}
													{#if canMove(lane, item)}
														<MenuItem icon="⤒" disabled={lk || (i === 0 && (view.mode === 'top' || view.line !== 0))} onclick={() => { closeMenu(); doNext(lane, item.ref); }}>Do next</MenuItem>
														<MenuItem icon="↑" disabled={lk || !stepOrder(view, item.ref, -1)} onclick={() => { closeMenu(); step(lane, item.ref, -1); }}>Move up</MenuItem>
														<MenuItem icon="↓" disabled={lk || !stepOrder(view, item.ref, 1)} onclick={() => { closeMenu(); step(lane, item.ref, 1); }}>Move down</MenuItem>
														<MenuItem icon="⇄" disabled={lk} onclick={() => openReassign(lane, item.ref)}>Reassign…</MenuItem>
													{/if}
													{#if lane.can_park}
														<MenuItem icon="❙❙" disabled={lk} onclick={() => { closeMenu(); void park(lane, { before: item.ref }); }}>Park before this</MenuItem>
														{#if lane.park}<MenuItem icon="▶" disabled={lk} onclick={() => { closeMenu(); void unpark(lane); }}>Unpark</MenuItem>{/if}
													{/if}
												{/if}
											</Menu>
										{/if}
									</li>
									{#if view.line === i + 1}{@render lineRule(lane, view)}{/if}
								{/each}
								{#if !compact && (lane.can_reorder || lane.can_park) && view.items.length}
									<li class="end-drop" class:drop={dropAt === `${key}#`} aria-hidden="true" ondragover={(e) => dragOver(e, lane, '')} ondrop={(e) => drop(e, lane, '')}></li>
								{/if}
							</ul>
							{#if !view.items.length}<p class="more">Nothing queued.</p>{/if}
							{#if lane.queue_more}<p class="show-all"><button type="button" class="link" onclick={() => agentsOverview.expandLane(key)}>Show all {lane.queue_total}</button></p>{/if}
							{#if finished.length}
								<h4>Just finished</h4>
								<ul>
									{#each finished as f (f.ref)}
										<li class="row finished"><span class="check" aria-hidden="true">✓</span>{@render refLink(f.owner_username, f.workspace, f.collection, f.ref)}<span class="title">{f.title}</span><span class="age">{f.value} {age(f.completed_at)} ago</span></li>
									{/each}
								</ul>
							{/if}
							{#if !lane.can_reorder && !lane.can_park}<p class="note">You can see this lane but not reorder or park it.</p>{/if}
						{/if}
					</section>
				{/each}
			</div>
		</div>
	{/if}
</li>

<style>
	.agent { border-top: 1px solid var(--border); list-style: none; }
	.agent:first-child { border-top: none; }
	.strip { display: grid; grid-template-columns: 16px 22px 140px minmax(0, 1fr) auto 12px 96px 110px; align-items: center; gap: var(--space-3); width: 100%; padding: var(--space-3) var(--space-4); background: none; border: none; color: var(--text-primary); font: inherit; font-size: .85rem; text-align: left; cursor: pointer; }
	.strip:hover, .open > .strip { background: var(--bg-hover); }
	.caret { color: var(--text-muted); font-size: .7rem; }
	.name { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.now { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.label, .extra, .age, .queued, .lanes-count, .more, .note { color: var(--text-muted); font-size: .75rem; }
	.queued { color: var(--text-secondary); }
	.ref { font-family: var(--font-mono); font-size: .75rem; color: var(--text-muted); }
	a.ref { text-decoration: none; border-radius: var(--radius-sm); }
	a.ref:hover { text-decoration: underline; }
	a.ref:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 1px; }
	.idle { color: var(--text-muted); }
	.ws, .chip { display: inline-block; border: 1px solid var(--border); border-radius: 99px; padding: 0 7px; font-size: .68rem; color: var(--text-secondary); background: var(--bg-tertiary); white-space: nowrap; }
	.chip.parked { color: var(--accent-orange); background: color-mix(in srgb, var(--accent-orange) 12%, transparent); border-color: color-mix(in srgb, var(--accent-orange) 35%, transparent); }
	.chip.parked.some { border-style: dashed; }
	.chip.blocked { color: var(--accent-red); background: color-mix(in srgb, var(--accent-red) 12%, transparent); border-color: transparent; }
	.chip.status { color: var(--accent-blue); background: color-mix(in srgb, var(--accent-blue) 10%, transparent); border-color: transparent; }
	.parked-slot { text-align: right; }
	.dot { width: 9px; height: 9px; border-radius: 50%; display: inline-block; }
	.dot.busy { background: var(--accent-green); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-green) 20%, transparent); }
	.dot.idle { background: var(--text-muted); }
	.dot.offline { border: 1.5px solid var(--text-muted); }
	.dot.none { visibility: hidden; }
	.detail { padding: var(--space-3) var(--space-4) var(--space-4); border-top: 1px solid var(--border); }
	.actions { display: flex; align-items: center; gap: var(--space-2); margin-bottom: var(--space-3); }
	.lanes-count { margin-left: auto; }
	.action { border: 1px solid var(--border); background: var(--bg-primary); color: var(--text-primary); border-radius: var(--radius-sm); padding: 5px 12px; font: inherit; font-size: .8rem; font-weight: 600; cursor: pointer; }
	.action:hover:not(:disabled) { background: var(--bg-hover); }
	.lanes { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); align-items: start; }
	.lanes.single { grid-template-columns: minmax(0, 1fr); }
	.lane { border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg-primary); overflow: hidden; }
	.lane-head { display: flex; align-items: center; gap: var(--space-2); width: 100%; padding: var(--space-2) var(--space-3); border: none; border-bottom: 1px solid var(--border); background: none; color: var(--text-primary); font: inherit; font-size: .85rem; text-align: left; }
	button.lane-head { cursor: pointer; }
	.lane-head .chip { margin-left: auto; }
	.lane-sum { margin-left: auto; color: var(--text-muted); font-size: .72rem; }
	.lane-sum + .chip { margin-left: var(--space-2); }
	.avatar { display: inline-flex; align-items: center; justify-content: center; width: 20px; height: 20px; border-radius: var(--radius-sm); color: var(--bg-primary); font-size: .7rem; font-weight: 700; }
	h4 { margin: 0; padding: var(--space-3) var(--space-3) var(--space-1); font-size: .66rem; text-transform: uppercase; letter-spacing: .03em; color: var(--text-muted); }
	ul { margin: 0; padding: 0; list-style: none; }
	.row { display: flex; align-items: center; gap: var(--space-2); padding: 6px var(--space-3); font-size: .8rem; min-height: 32px; }
	.row .title { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.title.strong { font-weight: 600; }
	.row.parked .title, .row.parked .ref, .row.parked .pos { color: var(--text-muted); }
	.row[draggable='true'] { cursor: grab; }
	.row.drop, .line.drop { box-shadow: inset 0 2px 0 var(--accent-blue); }
	.handle { width: 10px; color: var(--text-muted); font-size: .75rem; }
	.pos { width: 14px; text-align: right; color: var(--text-muted); font-size: .72rem; }
	.parked-tag { color: var(--text-muted); font-size: .7rem; }
	.prio { font-size: .72rem; white-space: nowrap; color: var(--text-secondary); }
	.prio.high, .prio.critical { color: var(--accent-orange); }
	.more-btn { border: 1px solid transparent; background: none; color: var(--text-muted); border-radius: var(--radius-sm); padding: 0 6px; cursor: pointer; font: inherit; }
	.more-btn:hover, .more-btn[aria-expanded='true'] { border-color: var(--border); background: var(--bg-hover); }
	.line { display: flex; align-items: center; gap: var(--space-2); padding: 4px var(--space-3); }
	.line .rule { flex: 1; border-top: 1.5px dashed var(--accent-orange); }
	.stop { display: inline-flex; align-items: center; gap: 4px; border: 1px solid var(--accent-orange); border-radius: 99px; padding: 1px 8px; font-size: .7rem; color: var(--text-secondary); background: color-mix(in srgb, var(--accent-orange) 10%, var(--bg-primary)); white-space: nowrap; }
	.stop strong { color: var(--accent-orange); }
	.stop.grab { cursor: grab; }
	.unpark-x { border: none; background: none; color: var(--text-muted); cursor: pointer; padding: 0 2px; font: inherit; }
	.end-drop { height: 8px; }
	.show-all { text-align: right; padding: var(--space-1) var(--space-3) var(--space-2); }
	.link { border: none; background: none; padding: 0; color: var(--accent-blue); font: inherit; font-size: .78rem; font-weight: 600; cursor: pointer; }
	.more, .note { padding: var(--space-1) var(--space-3) var(--space-2); margin: 0; }
	.note { border-top: 1px solid var(--border); padding-top: var(--space-2); }
	.check { color: var(--accent-green); }
	.finished .age { white-space: nowrap; }
	@media (max-width: 768px) {
		.strip { grid-template-columns: 14px 22px minmax(0, 1fr) auto auto 10px; grid-template-areas: 'caret icon name parked queued dot' '. . now now now age'; row-gap: 2px; padding: var(--space-3); }
		.caret { grid-area: caret; } .icon { grid-area: icon; } .name { grid-area: name; } .now { grid-area: now; font-size: .78rem; }
		.parked-slot { grid-area: parked; } .queued { grid-area: queued; } .dot { grid-area: dot; } .age { grid-area: age; text-align: right; }
		.now .label, .now .extra { display: none; }
		.detail { padding: var(--space-3); }
		.actions .action { flex: 1; }
		.lanes-count { display: none; }
		.lanes { grid-template-columns: 1fr; }
		.prio-word { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); }
	}
</style>
