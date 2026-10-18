<script lang="ts">
	// "Since you were last here" (TASK-3615, design DOC-3616): the caller's
	// catch-up digest on the workspace home. GET /catch-up never moves the
	// cursor; only Mark caught up does, and the card then refetches.
	//
	// IDENTITY FENCE, the BUG-3084 family's shape: the digest is about ONE
	// person, so a load that lands after a sign-in or sign-out must not paint
	// the previous user's digest. The load effect is keyed on
	// (sessionUserId, wsSlug) and drops the data before reloading (the
	// BUG-3701 class: key it on the route's workspace), and every commit point
	// checks both the identity epoch captured at entry and that the key is
	// still the one the work started under.
	//
	// Lines and agent groups are keyed by POSITION, not by ref or name: a
	// section is not de-duplicated by item on every path, and a repeated key
	// throws in production builds and blanks the view (BUG-3538 / TASK-3539).
	// The lists are replaced whole on every load, so a position key costs
	// nothing.
	//
	// LOAD ORDER: the key alone cannot order loads, since A→B→A returns to a
	// key an older A load still holds. Each load takes a sequence number and
	// only the newest may commit (TASK-3615 PR B r1).
	import { untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import type { CatchUpLine, CatchUpResponse, CatchUpSection } from '$lib/types';

	interface Props {
		username: string;
		wsSlug: string;
	}

	let { username, wsSlug }: Props = $props();

	let sessionUserId = $derived(authStore.userId);
	const key = $derived(`${sessionUserId}\n${wsSlug}`);

	let data = $state<CatchUpResponse | null>(null);
	let marking = $state(false);
	let markError = $state(false);
	// The mark landed but the refetch after it failed: the cursor moved, so
	// "couldn't mark" would be false, and clearing the card would hide that
	// the press worked.
	let refreshError = $state(false);
	let loadSeq = 0;

	function captureIdentity(): number {
		return authStore.identityEpoch;
	}

	function stillCurrent(captured: number, startedKey: string): boolean {
		return authStore.identityEpoch === captured && key === startedKey;
	}

	// A failed load clears the card unless keepOnError (the refetch after a
	// mark). A superseded one changes nothing.
	async function load(
		ws: string,
		startedKey: string,
		captured: number,
		keepOnError = false
	): Promise<'committed' | 'failed' | 'superseded'> {
		const seq = ++loadSeq;
		const committable = () => seq === loadSeq && stillCurrent(captured, startedKey);
		try {
			const res = await api.catchUp.get(ws);
			if (!committable()) return 'superseded';
			data = res;
			return 'committed';
		} catch {
			// The card is optional: a failed load leaves the home page as it was
			// and never shows an error over it. The next key change retries.
			if (!committable()) return 'superseded';
			if (!keepOnError) data = null;
			return 'failed';
		}
	}

	$effect(() => {
		const k = key;
		const ws = wsSlug;
		data = null;
		marking = false;
		markError = false;
		refreshError = false;
		if (!ws || !sessionUserId) return;
		untrack(() => void load(ws, k, captureIdentity()));
	});

	async function markCaughtUp() {
		const captured = captureIdentity();
		const startedKey = key;
		const ws = wsSlug;
		marking = true;
		markError = false;
		refreshError = false;
		try {
			await api.catchUp.markCaughtUp(ws);
		} catch {
			if (stillCurrent(captured, startedKey)) {
				markError = true;
				marking = false;
			}
			return;
		}
		try {
			if (!stillCurrent(captured, startedKey)) return;
			if ((await load(ws, startedKey, captured, true)) === 'failed') refreshError = true;
		} finally {
			if (stillCurrent(captured, startedKey)) marking = false;
		}
	}

	const sections = $derived.by(() => {
		if (!data) return [];
		const list: { id: string; label: string; section: CatchUpSection; window: boolean }[] = [
			{ id: 'needs_you', label: 'Needs you', section: data.needs_you, window: false },
			{ id: 'finished', label: 'Finished', section: data.finished, window: true },
			{ id: 'new_and_reopened', label: 'New and reopened', section: data.new_and_reopened, window: true },
			{ id: 'comments', label: 'Comments', section: data.comments, window: true }
		];
		return list.filter((s) => s.section.total > 0);
	});

	const needsYouDegraded = $derived(data?.degraded?.includes('needs_you') ?? false);
	const empty = $derived(!!data && sections.length === 0 && data.agents.total === 0 && !needsYouDegraded);

	const seeEverything = $derived(
		data
			? `/${username}/${wsSlug}/activity?since=${encodeURIComponent(data.window.since)}&view=audit`
			: ''
	);

	function clock(iso: string): string {
		const d = new Date(iso);
		const sameDay = d.toDateString() === new Date().toDateString();
		return sameDay
			? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
			: d.toLocaleString([], { weekday: 'long', hour: '2-digit', minute: '2-digit' });
	}

	const heading = $derived.by(() => {
		if (!data) return '';
		if (!data.window.has_cursor) return 'In the last 14 days';
		if (data.window.capped_at_ceiling) return 'Since you were last here · the last 14 days';
		return `Since you were last here · ${clock(data.window.since)}`;
	});

	const caughtUpLine = $derived.by(() => {
		if (!data) return '';
		if (!data.window.has_cursor) return "You're caught up · nothing new in the last 14 days";
		return `You're caught up · nothing new since ${clock(data.window.since)}`;
	});

	function meta(line: CatchUpLine): string {
		const parts = [line.what];
		const who = line.agent || line.actor;
		if (who) parts.push(who);
		if (line.at) parts.push(relativeTime(line.at));
		return parts.filter(Boolean).join(' · ');
	}

	function agentCounts(changes: number, items: number): string {
		return `${changes} ${changes === 1 ? 'change' : 'changes'} across ${items} ${items === 1 ? 'item' : 'items'}`;
	}

	function itemHref(line: CatchUpLine): string {
		return `/${username}/${wsSlug}/${line.collection}/${line.ref}`;
	}
</script>

{#snippet lineRow(line: CatchUpLine)}
	<li class="line">
		<a class="line-ref" href={itemHref(line)}>{line.ref}</a>
		<a class="line-title" href={itemHref(line)}>{line.title}</a>
		<span class="line-meta">{meta(line)}</span>
	</li>
{/snippet}

{#if data && empty}
	<section class="catch-up caught-up" data-testid="catch-up-card" aria-label="Catch up">
		<p class="caught-up-line" data-testid="catch-up-caught-up">{caughtUpLine}</p>
	</section>
{:else if data}
	<section class="catch-up" data-testid="catch-up-card" aria-labelledby="catch-up-heading">
		<header class="catch-up-header">
			<h2 id="catch-up-heading" class="catch-up-title">{heading}</h2>
			<button
				class="mark-caught-up"
				data-testid="catch-up-mark"
				disabled={marking}
				onclick={markCaughtUp}
			>
				{marking ? 'Marking…' : 'Mark caught up'}
			</button>
		</header>
		{#if markError}
			<p class="mark-error" role="alert">Couldn't mark this workspace caught up. Try again.</p>
		{:else if refreshError}
			<p class="mark-error" role="alert" data-testid="catch-up-refresh-error">
				Marked caught up, but this card couldn't refresh. Reload to see it.
			</p>
		{/if}

		{#if needsYouDegraded}
			<p class="degraded" data-testid="catch-up-degraded">Needs you is unavailable right now.</p>
		{/if}

		{#each sections as s (s.id)}
			<div class="catch-up-section" data-testid="catch-up-section-{s.id}">
				<h3 class="section-label">{s.label} ({s.section.total})</h3>
				<ul class="lines">
					{#each s.section.lines as line, i (i)}
						{@render lineRow(line)}
					{/each}
				</ul>
				{#if s.section.total > s.section.lines.length}
					{#if s.window}
						<a class="more" href={seeEverything}>… and {s.section.total - s.section.lines.length} more</a>
					{:else}
						<span class="more">… and {s.section.total - s.section.lines.length} more</span>
					{/if}
				{/if}
			</div>
		{/each}

		{#if data.agents.total > 0}
			<div class="catch-up-section" data-testid="catch-up-section-agents">
				<h3 class="section-label">Agents</h3>
				{#each data.agents.groups as g, gi (gi)}
					<details class="agent-group">
						<summary>
							<span class="agent-name">{g.agent}</span>
							<span class="agent-counts">{agentCounts(g.changes, g.items)}</span>
						</summary>
						<ul class="lines">
							{#each g.lines as line, i (i)}
								{@render lineRow(line)}
							{/each}
						</ul>
					</details>
				{/each}
			</div>
		{/if}

		<footer class="catch-up-footer">
			<a class="see-everything" href={seeEverything} data-testid="catch-up-see-everything">See everything →</a>
		</footer>
	</section>
{/if}

<style>
	.catch-up {
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
		margin-bottom: 1.5rem;
	}

	.caught-up {
		padding: 0.5rem 1rem;
	}

	.caught-up-line {
		margin: 0;
		font-size: 0.875rem;
		color: var(--text-secondary);
	}

	.catch-up-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
	}

	.catch-up-title {
		margin: 0;
		font-size: 0.9375rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.mark-caught-up {
		border: 1px solid var(--border);
		border-radius: 0.375rem;
		background: none;
		color: var(--text-primary);
		font-size: 0.8125rem;
		padding: 0.25rem 0.625rem;
		cursor: pointer;
		white-space: nowrap;
	}

	.mark-caught-up:disabled {
		cursor: default;
		opacity: 0.6;
	}

	.mark-error,
	.degraded {
		margin: 0.5rem 0 0;
		font-size: 0.8125rem;
		color: var(--text-secondary);
	}

	.catch-up-section {
		margin-top: 0.75rem;
	}

	.section-label {
		margin: 0 0 0.25rem;
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-secondary);
	}

	.lines {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.line {
		display: grid;
		grid-template-columns: auto 1fr auto;
		gap: 0.75rem;
		align-items: baseline;
		padding: 0.125rem 0 0.125rem 0.75rem;
		font-size: 0.875rem;
	}

	.line-ref {
		font-family: var(--font-mono, monospace);
		font-size: 0.8125rem;
		color: var(--text-secondary);
		text-decoration: none;
	}

	.line-title {
		color: var(--text-primary);
		text-decoration: none;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.line-title:hover,
	.line-ref:hover {
		text-decoration: underline;
	}

	.line-meta {
		color: var(--text-muted);
		font-size: 0.8125rem;
		white-space: nowrap;
	}

	.more {
		display: inline-block;
		padding-left: 0.75rem;
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.agent-group summary {
		display: flex;
		gap: 0.75rem;
		padding: 0.125rem 0 0.125rem 0.75rem;
		font-size: 0.875rem;
		cursor: pointer;
	}

	.agent-name {
		font-weight: 500;
		color: var(--text-primary);
	}

	.agent-counts {
		color: var(--text-muted);
	}

	.catch-up-footer {
		margin-top: 0.75rem;
		text-align: right;
	}

	.see-everything {
		font-size: 0.8125rem;
		color: var(--accent-primary);
		text-decoration: none;
	}

	@media (max-width: 640px) {
		.catch-up-header {
			flex-wrap: wrap;
		}

		.line {
			grid-template-columns: auto 1fr;
		}

		.line-meta {
			grid-column: 2;
			white-space: normal;
		}
	}
</style>
