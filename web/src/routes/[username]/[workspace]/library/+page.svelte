<script lang="ts">
	import { onDestroy, untrack } from 'svelte';
	import { ownValue } from '$lib/utils/ownValue';
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import Chip from '$lib/components/common/Chip.svelte';
	import { statusColor } from '$lib/utils/fieldColors';
	import type { LibraryCategory, LibraryConvention, PlaybookCategory, LibraryPlaybook, Item } from '$lib/types';

	/**
	 * IDENTITY FENCE — surface 3 of 7 (BUG-3084). Every async commit point on
	 * this page is checked against the signed-in identity, so a response or a
	 * click that lands after a sign-in or sign-out cannot commit the previous
	 * session's data. Same shape as the collection page (#1372/#1374), settings
	 * (#1370) and the roles board (#1375); the rules below are the family's,
	 * carried here rather than re-derived.
	 *
	 * WHAT IS DIFFERENT HERE. The population is 7, not the 3 a function-level
	 * count sees: four DEFERRED TIMERS (`setTimeout(… , 3000)` clearing the
	 * toast) are commit points that fire long after their handler returned, and
	 * the filed survey missed all four (BUG-3084 checkpoint 6).
	 *
	 * `activateConvention` / `activatePlaybook` COMPOSE A WRITE from state
	 * chosen under the previous identity — the `convention` / `playbook` the
	 * user clicked came from a list `loadData` fetched then. A click after an
	 * identity change is the current epoch, so every entry capture passes; the
	 * question that catches it is `pageIdentityHeld()`, exactly as on the roles
	 * board's drag handlers.
	 *
	 * `identityHeld(captured)` takes a handler's own ENTRY capture, because
	 * `loadData` re-stamps the page epoch and a handler comparing against that
	 * can be defeated by a concurrent load.
	 *
	 * `pageIdentityHeld()` asks whether this PAGE still belongs to the
	 * signed-in user.
	 */
	// `$state` for the same reason the roles board needs it: `pageIdentityHeld()`
	// is read from an `$effect` below, and an untracked value would leave that
	// effect with whichever answer it saw first.
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

	let categories = $state<LibraryCategory[]>([]);
	let playbookCategories = $state<PlaybookCategory[]>([]);
	let activeConventionTitles = $state<Set<string>>(new Set());
	let activePlaybookTitles = $state<Set<string>>(new Set());
	let loading = $state(true);

	// Scroll position restoration (BUG-1425). persistKey includes `?tab=…`
	// so the conventions and playbooks tabs each keep their own offset.
	const scrollRestoration = createScrollRestoration({
		// `loading` flips true on workspace change. Length-based gate
		// omitted (Codex P2 round 2).
		ready: () => !loading,
		persistKey: () =>
			wsSlug
				? `pad-last-scroll-${wsSlug}-${page.url.pathname}${page.url.search}`
				: null,
	});
	export const snapshot = scrollRestoration.snapshot;
	let activatingTitle = $state<string | null>(null);
	let toast = $state<string | null>(null);
	let activeTab = $state<'conventions' | 'playbooks'>(
		(page.url.searchParams.get('tab') === 'playbooks') ? 'playbooks' : 'conventions'
	);

	const categoryIcons: Record<string, string> = {
		git: '\u{1F500}',
		quality: '\u{2705}',
		pm: '\u{1F4CB}',
		docs: '\u{1F4DD}',
		build: '\u{1F527}',
		workflow: '\u{2699}\u{FE0F}',
		planning: '\u{1F4C5}',
		operations: '\u{1F680}',
	};

	const priorityColors: Record<string, string> = {
		must: 'var(--accent-orange)',
		should: 'var(--accent-amber)',
		'nice-to-have': 'var(--accent-gray)',
	};

	function conventionSurfaceLabel(convention: LibraryConvention): string {
		return convention.surfaces?.join(', ') || 'all';
	}

	/**
	 * Every piece of TRANSIENT INTERACTION STATE on this page — something a
	 * click started and a later response or timer would finish. Distinct from
	 * the page's DATA (the four collections `loadData` replaces) and from its
	 * identity bookkeeping.
	 *
	 * The source guard holds this against the file's own `$state` declarations,
	 * so a new piece of interaction state cannot arrive without being
	 * dispositioned here (BUG-3084, the roles board's checkpoint-14 lesson
	 * carried forward rather than re-learned).
	 */
	function resetTransientState() {
		activatingTitle = null;
		toast = null;
	}

	/**
	 * RE-LOAD ON AN IDENTITY CHANGE. The `$effect` below is keyed on `wsSlug`
	 * alone, so it does NOT re-run when the identity moves — and
	 * `routes/+layout.svelte` deliberately does not reload on an anonymous ->
	 * signed-in transition. Without this the page keeps a dead epoch and every
	 * `pageIdentityHeld()` is false for ever: both activate handlers go
	 * silently inert and never recover (#1372's regression, repaired in #1374).
	 *
	 * The reset runs FIRST: `activatingTitle` gates both handlers at their very
	 * first line, so a stale value latches them shut, and `toast` would keep the
	 * previous session's message on screen for whoever is signed in now.
	 */
	const stopIdentityWatch = authStore.onIdentityChange(() => {
		resetTransientState();
		if (wsSlug) void loadData(wsSlug);
	});
	onDestroy(stopIdentityWatch);

	$effect(() => {
		// UNTRACKED (codex round 1 [P2]). `loadData` reads `authStore.identityEpoch`
		// synchronously via `captureIdentity()` before its first await, so
		// without this the effect takes a dependency on the epoch and re-runs
		// when the identity moves — on top of the listener above, which already
		// re-loads for exactly that event. Two loads, eight requests, and
		// `loadGen` quietly discarding half of them: correct on screen, wasteful
		// on the wire, and the kind of thing that reads as a mystery in a
		// network log.
		//
		// The key this effect is FOR is the workspace. Settings (#1370) suppresses
		// the same class the same way; naming the real key is what makes the
		// suppression safe rather than a blanket silencing.
		const ws = wsSlug;
		if (ws) untrack(() => loadData(ws));
	});

	// Bumped by every `loadData()`. The identity fence and this ask different
	// questions and neither implies the other: the fence asks whether the
	// SIGNED-IN USER changed, this asks whether a NEWER LOAD for the same user
	// already landed. A workspace switch produces exactly the second.
	let loadGen = 0;

	async function loadData(ws: string) {
		const epochAtEntry = captureIdentity();
		const myLoad = ++loadGen;
		loading = true;
		try {
			const [libraryRes, playbookRes, existingConventions, existingPlaybooks] = await Promise.all([
				api.library.get(),
				api.library.getPlaybooks(),
				api.items.listByCollection(ws, 'conventions', { all: true }).catch(() => [] as Item[]),
				api.items.listByCollection(ws, 'playbooks', { all: true }).catch(() => [] as Item[]),
			]);
			if (!identityHeld(epochAtEntry)) return;
			if (myLoad !== loadGen) return;
			categories = libraryRes.categories;
			playbookCategories = playbookRes.categories;
			// THE SHARPEST READS ON THIS PAGE. These two sets are what the
			// activate handlers consult to decide whether a library entry is
			// already in the workspace, so a set belonging to the previous
			// session's workspace makes the NEW user's activate button either
			// dead or duplicating.
			activeConventionTitles = new Set(existingConventions.map((item) => item.title));
			activePlaybookTitles = new Set(existingPlaybooks.map((item) => item.title));
			// RE-STAMPED HERE, AFTER the data it vouches for has landed — never
			// before the await (BUG-3084, the roles board's checkpoint-14
			// lesson). `pageIdentityHeld()` means "this page's DATA belongs to
			// the signed-in user", and the activate handlers compose writes from
			// that data. Re-stamping at the top answers yes for the whole
			// round-trip while the four collections are still the previous
			// session's, so the guard would vouch for data it has not replaced.
			identityEpochAtLoad = epochAtEntry;
		} catch {
			if (!identityHeld(epochAtEntry)) return;
			if (myLoad !== loadGen) return;
			categories = [];
			playbookCategories = [];
			// CLEARED here too, because the re-stamp below vouches for whatever
			// is left and a failed load replaces neither set.
			activeConventionTitles = new Set();
			activePlaybookTitles = new Set();
			// RE-STAMPED ON THE ERROR PATH TOO: omitting it pins the page inert
			// for ever on a transient network error, which is the outage #1374
			// was opened to repair, and is the worse failure of the two.
			identityEpochAtLoad = epochAtEntry;
		} finally {
			// NOT identity-fenced: this must run on every exit path or the page
			// is pinned at its skeleton, and the flag discloses nothing about
			// either user. GENERATION-gated though, so only the NEWEST load may
			// declare the page loaded.
			if (myLoad === loadGen) loading = false;
		}
	}

	async function activateConvention(convention: LibraryConvention) {
		if (activeConventionTitles.has(convention.title) || activatingTitle) return;
		// BOTH QUESTIONS. `pageIdentityHeld()` first, for the reason the roles
		// board's drag handlers need it: the convention this writes was chosen
		// from a list `loadData` fetched under the PREVIOUS identity, and a
		// click after the change is the current epoch, so an entry capture
		// passes and the previous session's choice is activated into whatever
		// workspace is on screen now.
		if (!pageIdentityHeld()) return;
		const epochAtEntry = captureIdentity();
		activatingTitle = convention.title;
		try {
			await api.library.activate(wsSlug, convention);
			if (!identityHeld(epochAtEntry)) return;
			activeConventionTitles = new Set([...activeConventionTitles, convention.title]);
			toast = `Activated: ${convention.title}`;
			// FENCED, and the timer is the reason this surface's population is
			// 7 rather than 3: it commits 3 seconds after the handler returned,
			// by which time the identity may have moved. Clearing then would
			// wipe a toast belonging to whoever is signed in NOW. The identity
			// listener already clears `toast`, so the stale timer has nothing
			// legitimate left to do.
			setTimeout(() => {
				if (!identityHeld(epochAtEntry)) return;
				toast = null;
			}, 3000);
		} catch {
			if (!identityHeld(epochAtEntry)) return;
			toast = `Failed to activate: ${convention.title}`;
			setTimeout(() => {
				if (!identityHeld(epochAtEntry)) return;
				toast = null;
			}, 3000);
		} finally {
			// FENCED, unlike `loading` above, and the asymmetry is deliberate.
			// `activatingTitle` is cleared by `resetTransientState()` on every
			// identity change, so a stale continuation clearing it again can
			// only release a gate the NEW user's own click is holding — which
			// permits a second concurrent activation.
			if (identityHeld(epochAtEntry)) activatingTitle = null;
		}
	}

	async function activatePlaybook(playbook: LibraryPlaybook) {
		if (activePlaybookTitles.has(playbook.title) || activatingTitle) return;
		// BOTH QUESTIONS. `pageIdentityHeld()` first, for the reason the roles
		// board's drag handlers need it: the playbook this writes was chosen
		// from a list `loadData` fetched under the PREVIOUS identity, and a
		// click after the change is the current epoch, so an entry capture
		// passes and the previous session's choice is activated into whatever
		// workspace is on screen now.
		if (!pageIdentityHeld()) return;
		const epochAtEntry = captureIdentity();
		activatingTitle = playbook.title;
		try {
			await api.library.activatePlaybook(wsSlug, playbook);
			if (!identityHeld(epochAtEntry)) return;
			activePlaybookTitles = new Set([...activePlaybookTitles, playbook.title]);
			toast = `Activated: ${playbook.title}`;
			// See the timer note in activateConvention.
			setTimeout(() => {
				if (!identityHeld(epochAtEntry)) return;
				toast = null;
			}, 3000);
		} catch {
			if (!identityHeld(epochAtEntry)) return;
			toast = `Failed to activate: ${playbook.title}`;
			setTimeout(() => {
				if (!identityHeld(epochAtEntry)) return;
				toast = null;
			}, 3000);
		} finally {
			// See the note in activateConvention.
			if (identityHeld(epochAtEntry)) activatingTitle = null;
		}
	}

	function truncate(text: string, max: number): string {
		return text.length > max ? text.slice(0, max) + '...' : text;
	}

	function previewSteps(content: string): string {
		const lines = content.split('\n').filter((l) => l.match(/^\d+\./));
		return lines.slice(0, 3).join('\n');
	}
</script>

<div class="library">
	{#if loading}
		<div class="loading">Loading library...</div>
	{:else}
		<header class="library-header">
			<h1>Library</h1>
			<p class="subtitle">Pre-built conventions and playbooks to guide agent behavior. Activate the ones that fit your workflow.</p>
		</header>

		<div class="tabs">
			<button
				class="tab"
				class:active={activeTab === 'conventions'}
				onclick={() => (activeTab = 'conventions')}
			>
				Conventions
			</button>
			<button
				class="tab"
				class:active={activeTab === 'playbooks'}
				onclick={() => (activeTab = 'playbooks')}
			>
				Playbooks
			</button>
		</div>

		{#if activeTab === 'conventions'}
			{#if categories.length === 0}
				<p class="empty">No conventions available.</p>
			{/if}

			{#each categories as category (category.name)}
				<section class="category">
					<div class="category-header">
						<span class="category-icon">{categoryIcons[category.name] ?? '\u{1F4E6}'}</span>
						<div>
							<h2>{category.name}</h2>
							{#if category.description}
								<p class="category-desc">{category.description}</p>
							{/if}
						</div>
					</div>

					<div class="card-grid">
						{#each category.conventions as convention (convention.title)}
							{@const isActive = activeConventionTitles.has(convention.title)}
							{@const isActivating = activatingTitle === convention.title}
							<div class="card">
								<div class="card-body">
									<h3 class="card-title">{convention.title}</h3>
									<div class="badges">
										<Chip size="sm" color="var(--status-blue)">{convention.trigger}</Chip>
										<Chip size="sm" color="var(--accent-purple)">{conventionSurfaceLabel(convention)}</Chip>
										<Chip size="sm" color={ownValue(priorityColors, convention.enforcement) ?? 'var(--accent-gray)'}>
											{convention.enforcement}
										</Chip>
										{#if convention.commands?.length}
											<Chip size="sm" color="var(--accent-purple)">{convention.commands.length} cmd{convention.commands.length > 1 ? 's' : ''}</Chip>
										{/if}
									</div>
									<p class="card-content">{truncate(convention.content, 100)}</p>
								</div>
								<div class="card-action">
									{#if isActive}
										<Chip color={statusColor('active')}>Active</Chip>
									{:else}
										<button
											class="activate-btn"
											disabled={isActivating}
											onclick={() => activateConvention(convention)}
										>
											{#if isActivating}
												<span class="spinner"></span>
											{:else}
												Activate
											{/if}
										</button>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				</section>
			{/each}
		{:else}
			{#if playbookCategories.length === 0}
				<p class="empty">No playbooks available.</p>
			{/if}

			{#each playbookCategories as category (category.name)}
				<section class="category">
					<div class="category-header">
						<span class="category-icon">{categoryIcons[category.name] ?? '\u{1F4D8}'}</span>
						<div>
							<h2>{category.name}</h2>
							{#if category.description}
								<p class="category-desc">{category.description}</p>
							{/if}
						</div>
					</div>

					<div class="card-grid">
						{#each category.playbooks as playbook (playbook.title)}
							{@const isActive = activePlaybookTitles.has(playbook.title)}
							{@const isActivating = activatingTitle === playbook.title}
							<div class="card">
								<div class="card-body">
									<h3 class="card-title">{playbook.title}</h3>
									<div class="badges">
										{#if playbook.invocation_slug}
											<Chip size="sm" color="var(--accent-green)" title={`Run it by saying "run the ${playbook.invocation_slug} playbook" — or the shortcut for your agent: /pad ${playbook.invocation_slug} (Claude Code), $pad ${playbook.invocation_slug} (Codex), pad_playbook action=run ref=${playbook.invocation_slug} (MCP)`}><span class="slug-text">▶ {playbook.invocation_slug}</span></Chip>
										{/if}
										<Chip size="sm" color="var(--status-blue)">{playbook.trigger}</Chip>
										<Chip size="sm" color="var(--accent-purple)">{playbook.scope}</Chip>
										{#if playbook.arguments && playbook.arguments.length > 0}
											<Chip size="sm" color="var(--accent-amber)" title="Accepts {playbook.arguments.length} argument{playbook.arguments.length === 1 ? '' : 's'}">{playbook.arguments.length} arg{playbook.arguments.length === 1 ? '' : 's'}</Chip>
										{/if}
									</div>
									<p class="card-content card-steps">{previewSteps(playbook.content)}</p>
								</div>
								<div class="card-action">
									{#if isActive}
										<Chip color={statusColor('active')}>Active</Chip>
									{:else}
										<button
											class="activate-btn"
											disabled={isActivating}
											onclick={() => activatePlaybook(playbook)}
										>
											{#if isActivating}
												<span class="spinner"></span>
											{:else}
												Activate
											{/if}
										</button>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				</section>
			{/each}
		{/if}
	{/if}

	{#if toast}
		<div class="toast">{toast}</div>
	{/if}
</div>

<style>
	.library { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }
	.empty { color: var(--text-muted); text-align: center; padding-top: 10vh; }
	.library-header { margin-bottom: var(--space-6); }
	.library-header h1 { font-size: 1.6em; margin-bottom: var(--space-2); }
	.subtitle { color: var(--text-secondary); font-size: 0.95em; }

	.tabs {
		display: flex;
		gap: var(--space-1);
		margin-bottom: var(--space-8);
		border-bottom: 1px solid var(--border);
		padding-bottom: 0;
	}
	.tab {
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		color: var(--text-secondary);
		font-size: 0.95em;
		font-weight: 600;
		cursor: pointer;
		transition: color 0.15s, border-color 0.15s;
		margin-bottom: -1px;
	}
	.tab:hover { color: var(--text-primary); }
	.tab.active {
		color: var(--accent-blue);
		border-bottom-color: var(--accent-blue);
	}

	.category { margin-bottom: var(--space-8); }
	.category-header { display: flex; align-items: center; gap: var(--space-3); margin-bottom: var(--space-4); }
	.category-icon { font-size: 1.4em; }
	.category-header h2 { font-size: 1.1em; text-transform: capitalize; }
	.category-desc { font-size: 0.85em; color: var(--text-muted); margin-top: 2px; }

	.card-grid { display: grid; grid-template-columns: 1fr; gap: var(--space-3); }
	@media (min-width: 640px) {
		.card-grid { grid-template-columns: 1fr 1fr; }
	}

	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		justify-content: space-between;
		gap: var(--space-3);
		transition: border-color 0.15s;
	}
	.card:hover { border-color: var(--accent-blue); }

	.card-body { display: flex; flex-direction: column; gap: var(--space-2); }
	.card-title { font-size: 0.95em; font-weight: 600; }
	.card-content { font-size: 0.85em; color: var(--text-secondary); line-height: 1.5; }
	.card-steps { white-space: pre-line; }

	.badges { display: flex; flex-wrap: wrap; gap: var(--space-1); }
	/* PLAN-1377 invocation surface — slug chip signals "this playbook is callable as /pad <slug>". */
	.slug-text { font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace); }

	.card-action { display: flex; justify-content: flex-end; }

	.activate-btn {
		padding: var(--space-1) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-size: 0.8em;
		font-weight: 600;
		cursor: pointer;
		border: none;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		transition: opacity 0.15s;
	}
	.activate-btn:hover { opacity: 0.9; }
	.activate-btn:disabled { opacity: 0.6; cursor: not-allowed; }

	.spinner {
		width: 14px;
		height: 14px;
		border: 2px solid rgba(255, 255, 255, 0.3);
		border-top-color: #fff;
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}
	@keyframes spin { to { transform: rotate(360deg); } }

	.toast {
		position: fixed;
		bottom: var(--space-6);
		left: 50%;
		transform: translateX(-50%);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		padding: var(--space-3) var(--space-6);
		border-radius: var(--radius-lg);
		font-size: 0.85em;
		color: var(--text-primary);
		z-index: 100;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
		animation: fade-in 0.2s ease;
	}
	@keyframes fade-in { from { opacity: 0; transform: translateX(-50%) translateY(8px); } }
</style>
