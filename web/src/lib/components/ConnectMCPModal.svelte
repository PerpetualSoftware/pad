<script lang="ts">
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';

	// Per-client setup metadata for the "choose your client" grid.
	// Keep the entries in install-friction order (least → most setup).
	// Subtitles are intentionally short — one descriptor each, no marketing.
	// Doc URLs match the existing per-client pages on getpad.dev/docs/mcp.
	const CLIENTS: { id: string; name: string; subtitle: string; href: string }[] = [
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

	interface Props {
		open: boolean;
		// Required — the canonical URL the user pastes into their agent
		// client. Sourced upstream from authStore.mcpPublicUrl so the modal
		// works for any deployment that exposes a public MCP URL (Cloud or
		// self-host with PAD_MCP_PUBLIC_URL set), not just Pad Cloud.
		mcpPublicUrl: string;
		workspaceName?: string;
		// Optional — when present, footer renders a "Prefer the CLI? →"
		// affordance that closes this modal and asks the parent to open
		// the CLI install modal instead. Parent owns both modal states;
		// this component never directly mounts ConnectWorkspaceModal.
		onSwitchToCli?: () => void;
	}

	let {
		open = $bindable(),
		mcpPublicUrl,
		workspaceName = '',
		onSwitchToCli
	}: Props = $props();

	async function handleCopy() {
		const success = await copyToClipboard(mcpPublicUrl);
		if (success) {
			toastStore.show('Copied to clipboard', 'success');
		} else {
			toastStore.show('Failed to copy', 'error');
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && open) {
			open = false;
		}
	}

	function switchToCli() {
		open = false;
		onSwitchToCli?.();
	}

	// Documentation link target. Until TASK-1117 ships getpad.dev/mcp/remote,
	// fall back to the broader docs hub at /docs/mcp. Swap to /mcp/remote
	// once that page lands.
	const DOCS_HREF = 'https://getpad.dev/docs/mcp';
	const CONNECTED_APPS_HREF = '/console/connected-apps';
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => (open = false)}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				<h2>
					{#if workspaceName}
						Connect an AI agent to {workspaceName}
					{:else}
						Connect an AI agent to this workspace
					{/if}
				</h2>
				<button class="close-btn" type="button" onclick={() => (open = false)}>&#10005;</button>
			</div>

			<div class="modal-body">
				<p class="intro-copy">
					Paste this URL into any MCP-capable AI client. Sign in with your Pad
					account when prompted, and your agent can read and write everything
					in this workspace through natural conversation.
				</p>

				<!-- Step 1 — Copy URL -->
				<section class="step">
					<span class="section-label">Step 1 — Copy this URL</span>

					<div class="code-block">
						<pre>{mcpPublicUrl}</pre>
						<button
							class="copy-btn-small"
							type="button"
							title="Copy URL"
							onclick={handleCopy}
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

				<!-- Step 2 — Choose your client -->
				<section class="step">
					<span class="section-label">Step 2 — Set it up in your client</span>

					<div class="client-grid">
						{#each CLIENTS as client (client.id)}
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

				<!-- Footer links -->
				<div class="modal-footer-links">
					<a href={CONNECTED_APPS_HREF}>Connected agents</a>
					<span class="footer-sep">&middot;</span>
					<a href={DOCS_HREF} target="_blank" rel="noopener noreferrer">Documentation</a>
					{#if onSwitchToCli}
						<span class="footer-sep">&middot;</span>
						<button class="footer-link-btn" type="button" onclick={switchToCli}>
							Prefer the CLI? &rarr;
						</button>
					{/if}
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

	/* Code block — same shape as ConnectWorkspaceModal so the modals feel
	   like siblings. Single-line URL gets the same padding + monospace
	   treatment as the multi-line CLI install commands there. */
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

	.modal-footer-links {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: var(--space-2);
		padding-top: var(--space-3);
		border-top: 1px solid var(--border);
		font-size: 0.82em;
	}

	.modal-footer-links a,
	.modal-footer-links .footer-link-btn {
		color: var(--text-muted);
		text-decoration: none;
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		cursor: pointer;
	}

	.modal-footer-links a:hover,
	.modal-footer-links .footer-link-btn:hover {
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
	}
</style>
