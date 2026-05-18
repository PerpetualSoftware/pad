<script lang="ts">
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { defaultInstallTab, type InstallTab } from '$lib/utils/platform';
	import { api, PadApiError } from '$lib/api/client';
	import type { ClaimCodeResponse } from '$lib/types';

	interface Props {
		open: boolean;
		workspaceSlug: string;
		workspaceName?: string;
		// window.location.origin from the caller — sub-pages render
		// canonical URLs without baking a build-time origin into the bundle.
		// Used by the CLI tab to produce a `pad init --url <serverUrl>` snippet.
		serverUrl: string;
		// Empty string when the deployment doesn't expose a public MCP URL
		// (typical self-host without PAD_MCP_PUBLIC_URL). When empty, the
		// MCP tab is hidden entirely and the default tab falls through to CLI.
		mcpPublicUrl?: string;
	}

	let {
		open = $bindable(),
		workspaceSlug,
		workspaceName = '',
		serverUrl,
		mcpPublicUrl = ''
	}: Props = $props();

	// --- Primary tabs --------------------------------------------------------
	//
	// IDEA-1517 §4 spec: three primary connection paths in one modal. Order is
	// deliberate — claim code first because once a user has authorized any agent
	// on any workspace via OAuth, claim-code is dramatically the lowest-friction
	// path to add additional workspaces. MCP setup is the "fresh agent" path.
	// CLI is the terminal-only escape hatch (and the only flow that works on
	// self-host deployments without a public MCP URL).
	type PrimaryTab = 'agent' | 'mcp' | 'cli';

	const primaryTabs: { id: PrimaryTab; label: string; subtitle: string }[] = [
		{ id: 'agent', label: 'Agent', subtitle: 'Claim a workspace (recommended)' },
		{ id: 'mcp', label: 'MCP setup', subtitle: 'Authorize a new agent' },
		{ id: 'cli', label: 'CLI', subtitle: 'Terminal access' }
	];

	// Default-tab logic. Two cases:
	//   1. Deployment has a public MCP URL → claim-code is the high-value path
	//      because users typically already have one OAuth-authorized agent and
	//      just want this workspace added to that grant. Default to 'agent'.
	//   2. No public MCP URL (self-host without remote MCP) → claim-code AND
	//      the MCP tab are both meaningless (the claim flow only matters in
	//      the context of an OAuth grant on the remote MCP). Default to 'cli'.
	//
	// We compute this as a $derived rather than $state because we want it to
	// reset to the appropriate default each time `open` flips true — using
	// a $derived keyed off `open` would still latch the wrong value if the
	// user manually switched tabs, so we use a $state seeded by an $effect
	// that fires only on the open→true transition.
	let activeTab = $state<PrimaryTab>('cli');
	let lastOpen = $state(false);
	$effect(() => {
		if (open && !lastOpen) {
			activeTab = mcpPublicUrl ? 'agent' : 'cli';
		}
		lastOpen = open;
	});

	// --- CLI tab state -------------------------------------------------------

	// Active install tab persists across open/close cycles — initialized
	// from the detected platform on first mount.
	let installTab = $state<InstallTab>(defaultInstallTab());

	// All commands mirror the README's Installation section. Homebrew works
	// on both macOS and Linux. Windows has no first-party one-liner — direct
	// users to the GitHub releases page. Docker matches the documented run
	// invocation (data volume at /data, port published to localhost).
	const installCommands: Record<InstallTab, string> = {
		macos: 'brew install PerpetualSoftware/tap/pad',
		linux: 'brew install PerpetualSoftware/tap/pad',
		windows: '# Download a Windows binary from\n# https://github.com/PerpetualSoftware/pad/releases',
		docker: 'docker run -p 127.0.0.1:7777:7777 -v pad-data:/data ghcr.io/perpetualsoftware/pad'
	};

	const installTabs: { id: InstallTab; label: string }[] = [
		{ id: 'macos', label: 'macOS' },
		{ id: 'linux', label: 'Linux' },
		{ id: 'windows', label: 'Windows' },
		{ id: 'docker', label: 'Docker' }
	];

	let connectSnippet = $derived(
		`cd /path/to/your/project\npad init --url ${serverUrl} --workspace ${workspaceSlug}`
	);

	// --- MCP tab data --------------------------------------------------------

	// Per-client setup metadata for the "choose your client" grid.
	// Doc URLs match the existing per-client pages on getpad.dev/docs/mcp.
	const MCP_CLIENTS: { id: string; name: string; subtitle: string; href: string }[] = [
		{
			id: 'claude-desktop',
			name: 'Claude Desktop',
			subtitle: 'Web + Desktop',
			href: 'https://getpad.dev/docs/mcp/claude-desktop'
		},
		{
			id: 'cursor',
			name: 'Cursor',
			subtitle: 'AI code editor',
			href: 'https://getpad.dev/docs/mcp/cursor'
		},
		{
			id: 'windsurf',
			name: 'Windsurf',
			subtitle: 'AI code editor',
			href: 'https://getpad.dev/docs/mcp/windsurf'
		},
		{
			id: 'chatgpt',
			name: 'ChatGPT',
			subtitle: 'Connectors (Pro/Enterprise)',
			href: 'https://getpad.dev/docs/mcp/chatgpt'
		}
	];

	// --- Claim code state ----------------------------------------------------
	//
	// We refetch under three conditions: (a) the user opens the modal on the
	// agent tab, (b) the user switches TO the agent tab, (c) the current code
	// expires. The TTL is server-controlled (5-minute bucket aligned), and
	// the response includes the exact RFC3339 `expires_at` — we drive a local
	// countdown off that and treat the bucket boundary as the refetch trigger.
	//
	// Suppression case: when an existing OAuth grant has already claimed this
	// workspace, the server replies with `suppressed: true` and no `code`.
	// We render a calm "already connected" panel instead of a code, because
	// generating a new code in that state would be misleading — the grant the
	// user is currently looking at would be the one being augmented, and that
	// is precisely what they already have. Surfacing the existing grant by
	// name (when available) gives them an immediate, accurate next step.
	let claimState = $state<
		| { kind: 'idle' }
		| { kind: 'loading' }
		| { kind: 'ready'; data: ClaimCodeResponse }
		| { kind: 'suppressed'; grantName: string }
		| { kind: 'disabled' }
		| { kind: 'error'; message: string }
	>({ kind: 'idle' });

	let secondsRemaining = $state(0);
	let countdownTimer: ReturnType<typeof setInterval> | null = null;

	function clearCountdown() {
		if (countdownTimer) {
			clearInterval(countdownTimer);
			countdownTimer = null;
		}
	}

	function startCountdown(expiresAt: string) {
		clearCountdown();
		const tick = () => {
			const remaining = Math.max(
				0,
				Math.floor((new Date(expiresAt).getTime() - Date.now()) / 1000)
			);
			secondsRemaining = remaining;
			if (remaining <= 0) {
				clearCountdown();
				// Bucket boundary crossed — refetch silently. The server will
				// hand us the next bucket's code (same flow as initial fetch).
				void fetchClaimCode();
			}
		};
		tick();
		countdownTimer = setInterval(tick, 1000);
	}

	// Monotonic sequence id for in-flight claim-code requests. The
	// modal stays mounted across workspace switches (TopBar reuses
	// the same instance as the user navigates between workspaces),
	// so an older fetch can resolve AFTER a newer one and stomp
	// `claimState` with stale suppression / a code for the wrong
	// workspace. Each fetchClaimCode() captures its own seq + the
	// slug it was issued for; the state write only commits when
	// BOTH still match the latest values. Mirrors the
	// refreshHasAgentActivity guard in ConnectBanner.svelte.
	let claimFetchSeq = 0;

	async function fetchClaimCode() {
		if (!workspaceSlug) return;
		const mySeq = ++claimFetchSeq;
		const mySlug = workspaceSlug;
		claimState = { kind: 'loading' };
		try {
			const data = await api.workspaces.claimCode(mySlug);
			// Drop the response if a newer fetch has been issued OR the
			// user navigated to a different workspace while we were
			// awaiting the network.
			if (mySeq !== claimFetchSeq || mySlug !== workspaceSlug) return;
			if (data.suppressed) {
				clearCountdown();
				claimState = {
					kind: 'suppressed',
					grantName: data.suppression_grant_name ?? ''
				};
				return;
			}
			claimState = { kind: 'ready', data };
			startCountdown(data.expires_at);
		} catch (err) {
			if (mySeq !== claimFetchSeq || mySlug !== workspaceSlug) return;
			clearCountdown();
			if (err instanceof PadApiError && err.code === 'claim_disabled') {
				claimState = { kind: 'disabled' };
			} else {
				const message =
					err instanceof Error && err.message
						? err.message
						: 'Couldn’t generate a claim code right now.';
				claimState = { kind: 'error', message };
			}
		}
	}

	// Fetch when the modal opens on (or switches to) the agent tab. We gate
	// on `open && activeTab === 'agent'` so we don't burn an API call for users
	// who never look at this tab. Re-fetching when workspaceSlug changes
	// matters because the modal stays mounted as a singleton in some callers.
	$effect(() => {
		if (open && activeTab === 'agent' && workspaceSlug) {
			void fetchClaimCode();
		}
		if (!open || activeTab !== 'agent') {
			clearCountdown();
		}
	});

	// Clean up the interval if the component is destroyed mid-countdown.
	$effect(() => {
		return () => clearCountdown();
	});

	// Derived display values for the claim tab.
	let claimCodeDigits = $derived.by(() => {
		if (claimState.kind !== 'ready') return '';
		return claimState.data.code ?? '';
	});

	// Spaced display: "123 456" reads better than "123456".
	let claimCodeDisplay = $derived.by(() => {
		const c = claimCodeDigits;
		if (c.length !== 6) return c;
		return `${c.slice(0, 3)} ${c.slice(3)}`;
	});

	let countdownDisplay = $derived.by(() => {
		const m = Math.floor(secondsRemaining / 60);
		const s = secondsRemaining % 60;
		return `${m}:${s.toString().padStart(2, '0')}`;
	});

	// The agent prompt is locked verbatim per spec — single quotes around
	// the slug, period at the end. Don't reflow or rewrite this; downstream
	// agents may pattern-match against it.
	let claimPrompt = $derived(
		`Authorize the pad workspace '${workspaceSlug}' with claim code ${claimCodeDigits}.`
	);

	// --- Copy helpers --------------------------------------------------------

	async function handleCopy(text: string, label = 'Copied to clipboard') {
		const success = await copyToClipboard(text);
		toastStore.show(success ? label : 'Failed to copy', success ? 'success' : 'error');
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && open) {
			open = false;
		}
	}

	// --- Display helpers -----------------------------------------------------

	let title = $derived(
		workspaceName
			? `Connect ${workspaceName} to your AI agent`
			: 'Connect this workspace to your AI agent'
	);

	// Hide MCP tab on deployments without a public MCP URL — the tab would
	// have nothing meaningful to show.
	let visibleTabs = $derived(
		primaryTabs.filter((t) => t.id !== 'mcp' || !!mcpPublicUrl)
	);

	const CONNECTED_APPS_HREF = '/console/connected-apps';
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => (open = false)}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				<h2>{title}</h2>
				<button class="close-btn" type="button" onclick={() => (open = false)}>&#10005;</button>
			</div>

			<div class="modal-body">
				<!-- Primary tab strip -->
				<div class="primary-tabs" role="tablist">
					{#each visibleTabs as tab (tab.id)}
						<button
							class="primary-tab"
							class:active={activeTab === tab.id}
							role="tab"
							aria-selected={activeTab === tab.id}
							type="button"
							onclick={() => (activeTab = tab.id)}
						>
							<span class="primary-tab-label">{tab.label}</span>
							<span class="primary-tab-subtitle">{tab.subtitle}</span>
						</button>
					{/each}
				</div>

				{#if activeTab === 'agent'}
					<!-- Agent / claim-code panel -->
					{#if claimState.kind === 'loading' || claimState.kind === 'idle'}
						<div class="claim-skeleton" aria-busy="true">
							<div class="skeleton-block skeleton-code"></div>
							<div class="skeleton-block skeleton-prompt"></div>
						</div>
					{:else if claimState.kind === 'suppressed'}
						<div class="info-panel">
							<p class="info-panel-title">
								{#if claimState.grantName}
									Your agent <strong>{claimState.grantName}</strong> is already connected to this workspace.
								{:else}
									Your agent is already connected to this workspace.
								{/if}
							</p>
							<p class="info-panel-body">Manage it from your Connected apps page.</p>
							<a class="info-panel-link" href={CONNECTED_APPS_HREF}>
								Open Connected apps &rarr;
							</a>
						</div>
					{:else if claimState.kind === 'disabled'}
						<div class="info-panel">
							<p class="info-panel-body">
								Claim codes aren’t available on this deployment. Use the
								<button
									class="inline-link-btn"
									type="button"
									onclick={() => mcpPublicUrl && (activeTab = 'mcp')}
									disabled={!mcpPublicUrl}
								>MCP tab</button>
								to authorize an agent from scratch, or the
								<button
									class="inline-link-btn"
									type="button"
									onclick={() => (activeTab = 'cli')}
								>CLI tab</button>
								for terminal access.
							</p>
						</div>
					{:else if claimState.kind === 'error'}
						<div class="info-panel">
							<p class="info-panel-body">
								Couldn’t generate a claim code right now. Try again.
							</p>
							<button class="retry-btn" type="button" onclick={() => void fetchClaimCode()}>
								Try again
							</button>
						</div>
					{:else}
						<!-- ready -->
						<section class="step">
							<span class="section-label">Step 1 — Read your agent the code</span>
							<div class="claim-code-row">
								<div class="claim-code">{claimCodeDisplay}</div>
								<button
									class="copy-btn-small"
									type="button"
									title="Copy code"
									onclick={() => handleCopy(claimCodeDigits, 'Code copied')}
								>
									<svg
										width="14"
										height="14"
										viewBox="0 0 24 24"
										fill="none"
										stroke="currentColor"
										stroke-width="2"
										stroke-linecap="round"
										stroke-linejoin="round"
									>
										<rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
										<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
									</svg>
								</button>
							</div>
							<div class="countdown">Expires in {countdownDisplay}</div>
						</section>

						<section class="step">
							<span class="section-label">Step 2 — Or paste this exact prompt into your agent</span>
							<div class="code-block">
								<pre>{claimPrompt}</pre>
								<button
									class="copy-btn-small"
									type="button"
									title="Copy prompt"
									onclick={() => handleCopy(claimPrompt, 'Prompt copied')}
								>
									<svg
										width="14"
										height="14"
										viewBox="0 0 24 24"
										fill="none"
										stroke="currentColor"
										stroke-width="2"
										stroke-linecap="round"
										stroke-linejoin="round"
									>
										<rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
										<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
									</svg>
								</button>
							</div>
							<p class="hint">
								Your agent will use the code to add this workspace to its grant — no
								re-authorization needed.
							</p>
						</section>
					{/if}
				{:else if activeTab === 'mcp'}
					<!-- MCP setup panel -->
					{#if !mcpPublicUrl}
						<div class="info-panel">
							<p class="info-panel-body">
								Remote MCP isn’t enabled on this deployment. Use the
								<button
									class="inline-link-btn"
									type="button"
									onclick={() => (activeTab = 'cli')}
								>CLI tab</button>
								to connect via terminal.
							</p>
						</div>
					{:else}
						<p class="intro-copy">
							Paste this URL into any MCP-capable AI client. Sign in with your Pad
							account when prompted, and your agent can read and write everything
							in this workspace through natural conversation.
						</p>

						<section class="step">
							<span class="section-label">Step 1 — Copy this URL</span>
							<div class="code-block">
								<pre>{mcpPublicUrl}</pre>
								<button
									class="copy-btn-small"
									type="button"
									title="Copy URL"
									onclick={() => handleCopy(mcpPublicUrl)}
								>
									<svg
										width="14"
										height="14"
										viewBox="0 0 24 24"
										fill="none"
										stroke="currentColor"
										stroke-width="2"
										stroke-linecap="round"
										stroke-linejoin="round"
									>
										<rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
										<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
									</svg>
								</button>
							</div>
						</section>

						<section class="step">
							<span class="section-label">Step 2 — Set it up in your client</span>
							<div class="client-grid">
								{#each MCP_CLIENTS as client (client.id)}
									<a
										class="client-card"
										href={client.href}
										target="_blank"
										rel="noopener noreferrer"
									>
										<div class="client-card-text">
											<span class="client-name">{client.name}</span>
											<span class="client-subtitle">{client.subtitle}</span>
										</div>
										<svg
											class="client-arrow"
											width="14"
											height="14"
											viewBox="0 0 24 24"
											fill="none"
											stroke="currentColor"
											stroke-width="2"
											stroke-linecap="round"
											stroke-linejoin="round"
											aria-hidden="true"
										>
											<path d="M7 17L17 7" />
											<path d="M7 7h10v10" />
										</svg>
									</a>
								{/each}
							</div>
						</section>
					{/if}
				{:else}
					<!-- CLI panel -->
					<p class="intro-copy">
						{#if workspaceName}
							Run <strong>{workspaceName}</strong> from your terminal with the pad CLI.
						{:else}
							Run this workspace from your terminal with the pad CLI.
						{/if}
					</p>

					<section class="step">
						<span class="section-label">Step 1 — Install pad</span>

						<div class="tab-strip" role="tablist">
							{#each installTabs as tab (tab.id)}
								<button
									class="tab-btn"
									class:active={installTab === tab.id}
									role="tab"
									aria-selected={installTab === tab.id}
									type="button"
									onclick={() => (installTab = tab.id)}
								>
									{tab.label}
								</button>
							{/each}
						</div>

						<div class="code-block">
							<pre>{installCommands[installTab]}</pre>
							<button
								class="copy-btn-small"
								type="button"
								title="Copy command"
								onclick={() => handleCopy(installCommands[installTab])}
							>
								<svg
									width="14"
									height="14"
									viewBox="0 0 24 24"
									fill="none"
									stroke="currentColor"
									stroke-width="2"
									stroke-linecap="round"
									stroke-linejoin="round"
								>
									<rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
									<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
								</svg>
							</button>
						</div>

						<a
							class="other-options"
							href="https://getpad.dev/docs#installation"
							target="_blank"
							rel="noopener noreferrer"
						>
							Other install options &rarr;
						</a>
					</section>

					<section class="step">
						<span class="section-label">Step 2 — Connect this workspace</span>
						<div class="code-block">
							<pre>{connectSnippet}</pre>
							<button
								class="copy-btn-small"
								type="button"
								title="Copy snippet"
								onclick={() => handleCopy(connectSnippet)}
							>
								<svg
									width="14"
									height="14"
									viewBox="0 0 24 24"
									fill="none"
									stroke="currentColor"
									stroke-width="2"
									stroke-linecap="round"
									stroke-linejoin="round"
								>
									<rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
									<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
								</svg>
							</button>
						</div>
					</section>
				{/if}

				<!-- Footer links -->
				<div class="modal-footer-links">
					<a
						href="https://getpad.dev/docs/connect-workspace"
						target="_blank"
						rel="noopener noreferrer"
					>
						Documentation
					</a>
					<span class="footer-sep">&middot;</span>
					<a
						href="https://getpad.dev/docs/connect-workspace#troubleshooting"
						target="_blank"
						rel="noopener noreferrer"
					>
						Troubleshooting
					</a>
					<span class="footer-sep">&middot;</span>
					<a href={CONNECTED_APPS_HREF}>Connected agents &rarr;</a>
				</div>
			</div>
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		align-items: flex-start;
		padding-top: 10vh;
	}

	.modal {
		width: 100%;
		max-width: 560px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		overflow: hidden;
		max-height: 85vh;
		display: flex;
		flex-direction: column;
	}

	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.modal-header h2 {
		margin: 0;
		font-size: 1.1em;
		font-weight: 600;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		flex-shrink: 0;
	}

	.close-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.modal-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		overflow-y: auto;
	}

	.intro-copy {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-secondary);
		line-height: 1.5;
	}

	.intro-copy strong {
		color: var(--text-primary);
		font-weight: 600;
	}

	.section-label {
		display: block;
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-3);
	}

	.step {
		display: flex;
		flex-direction: column;
	}

	.hint {
		margin: var(--space-2) 0 0;
		font-size: 0.82em;
		color: var(--text-muted);
		line-height: 1.5;
	}

	/* Primary tab strip — larger, two-line cards because each tab represents
	   a distinct flow rather than a platform variant. The label gets accent
	   color treatment and the subtitle stays muted so the user can scan. */
	.primary-tabs {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
	}

	.primary-tab {
		flex: 1 1 0;
		min-width: 0;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: 2px;
		padding: var(--space-3);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		cursor: pointer;
		text-align: left;
		transition: border-color 0.15s, color 0.15s, background 0.15s;
	}

	.primary-tab:hover {
		border-color: var(--accent-blue);
	}

	.primary-tab.active {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
	}

	.primary-tab-label {
		font-size: 0.9em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.primary-tab.active .primary-tab-label {
		color: var(--accent-blue);
	}

	.primary-tab-subtitle {
		font-size: 0.75em;
		color: var(--text-muted);
	}

	/* Install-platform tab strip — flat, bordered buttons. */
	.tab-strip {
		display: flex;
		gap: var(--space-1);
		margin-bottom: var(--space-3);
		flex-wrap: wrap;
	}

	.tab-btn {
		padding: var(--space-1) var(--space-3);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.82em;
		font-weight: 500;
		cursor: pointer;
		white-space: nowrap;
		transition: border-color 0.15s, color 0.15s, background 0.15s;
	}

	.tab-btn:hover {
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	.tab-btn.active {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
		color: var(--accent-blue);
	}

	/* Code block — monospace, padded, with copy button on the right. */
	.code-block {
		position: relative;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		padding: var(--space-3) var(--space-9) var(--space-3) var(--space-3);
		min-height: 0;
	}

	.code-block pre {
		margin: 0;
		font-family: var(--font-mono);
		font-size: 0.82em;
		color: var(--text-primary);
		white-space: pre-wrap;
		word-break: break-all;
		line-height: 1.5;
	}

	.copy-btn-small {
		position: absolute;
		top: var(--space-2);
		right: var(--space-2);
		display: flex;
		align-items: center;
		justify-content: center;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: 4px;
		border-radius: var(--radius-sm);
		flex-shrink: 0;
	}

	.copy-btn-small:hover {
		color: var(--accent-blue);
		background: var(--bg-hover);
	}

	/* Claim code display — large, monospaced, with prominent letter-spacing
	   so users can read it aloud without losing their place. */
	.claim-code-row {
		position: relative;
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		padding: var(--space-4) var(--space-9) var(--space-4) var(--space-4);
	}

	.claim-code {
		font-family: var(--font-mono);
		font-size: 2em;
		font-weight: 600;
		letter-spacing: 0.15em;
		color: var(--text-primary);
	}

	.countdown {
		margin-top: var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
	}

	.claim-skeleton {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.skeleton-block {
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		animation: skeleton-pulse 1.4s ease-in-out infinite;
	}

	.skeleton-code {
		height: 4rem;
	}

	.skeleton-prompt {
		height: 5rem;
	}

	@keyframes skeleton-pulse {
		0%, 100% { opacity: 0.6; }
		50% { opacity: 1; }
	}

	/* Info panel — suppressed / disabled / error states all share the same
	   calm presentation. Distinct from the code-block because it's prose,
	   not something to copy. */
	.info-panel {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-4);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}

	.info-panel-title {
		margin: 0;
		font-size: 0.95em;
		color: var(--text-primary);
		line-height: 1.5;
	}

	.info-panel-body {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-secondary);
		line-height: 1.5;
	}

	.info-panel-link {
		margin-top: var(--space-1);
		font-size: 0.9em;
		color: var(--accent-blue);
		text-decoration: none;
		align-self: flex-start;
	}

	.info-panel-link:hover {
		text-decoration: underline;
	}

	.inline-link-btn {
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		color: var(--accent-blue);
		cursor: pointer;
		text-decoration: underline;
	}

	.inline-link-btn:disabled {
		color: var(--text-muted);
		cursor: default;
		text-decoration: none;
	}

	.retry-btn {
		align-self: flex-start;
		margin-top: var(--space-1);
		padding: var(--space-1) var(--space-3);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.82em;
		cursor: pointer;
	}

	.retry-btn:hover {
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	/* Client grid — 2-col on desktop, 1-col on narrow viewports. */
	.client-grid {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: var(--space-2);
	}

	.client-card {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s, background 0.15s;
	}

	.client-card:hover {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-tertiary));
	}

	.client-card-text {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.client-name {
		font-size: 0.9em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.client-subtitle {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	.client-arrow {
		color: var(--text-muted);
		flex-shrink: 0;
	}

	.client-card:hover .client-arrow {
		color: var(--accent-blue);
	}

	.other-options {
		display: inline-block;
		margin-top: var(--space-3);
		font-size: 0.82em;
		color: var(--text-muted);
		text-decoration: none;
		align-self: flex-start;
	}

	.other-options:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	.modal-footer-links {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-2);
		padding-top: var(--space-3);
		border-top: 1px solid var(--border);
		font-size: 0.82em;
	}

	.modal-footer-links a {
		color: var(--text-muted);
		text-decoration: none;
	}

	.modal-footer-links a:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	.footer-sep {
		color: var(--border);
	}

	@media (max-width: 480px) {
		.modal-header h2 {
			white-space: normal;
			font-size: 1em;
		}
		.client-grid {
			grid-template-columns: 1fr;
		}
		.claim-code {
			font-size: 1.8em;
		}
	}
</style>
