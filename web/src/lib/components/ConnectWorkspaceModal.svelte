<script lang="ts">
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { defaultInstallTab, type InstallTab } from '$lib/utils/platform';

	interface Props {
		open: boolean;
		serverUrl: string;
		workspaceSlug: string;
		workspaceName?: string;
	}

	let {
		open = $bindable(),
		serverUrl,
		workspaceSlug,
		workspaceName = ''
	}: Props = $props();

	// Active install tab persists across open/close cycles — initialized
	// from the detected platform on first mount.
	let activeTab = $state<InstallTab>(defaultInstallTab());

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

	const tabs: { id: InstallTab; label: string }[] = [
		{ id: 'macos', label: 'macOS' },
		{ id: 'linux', label: 'Linux' },
		{ id: 'windows', label: 'Windows' },
		{ id: 'docker', label: 'Docker' }
	];

	let connectSnippet = $derived(
		`cd /path/to/your/project\npad init --url ${serverUrl} --workspace ${workspaceSlug}`
	);

	async function handleCopy(text: string) {
		const success = await copyToClipboard(text);
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

	// Docs URLs point at getpad.dev/docs (TASK-863 / pad-web#30). The
	// "Connect a Workspace" guide has its own #troubleshooting anchor;
	// "Other install options" goes to the broader /docs#installation
	// section which lists Homebrew, Docker, binary download, and source.
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => (open = false)}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				<h2>Connect this workspace to your local project</h2>
				<button class="close-btn" type="button" onclick={() => (open = false)}>&#10005;</button>
			</div>

			<div class="modal-body">
				<p class="intro-copy">
					{#if workspaceName}
						Run <strong>{workspaceName}</strong> from your terminal with the pad CLI.
					{:else}
						Run this workspace from your terminal with the pad CLI.
					{/if}
				</p>

				<!-- Step 1 — Install pad -->
				<section class="step">
					<span class="section-label">Step 1 — Install pad</span>

					<div class="tab-strip" role="tablist">
						{#each tabs as tab (tab.id)}
							<button
								class="tab-btn"
								class:active={activeTab === tab.id}
								role="tab"
								aria-selected={activeTab === tab.id}
								type="button"
								onclick={() => (activeTab = tab.id)}
							>
								{tab.label}
							</button>
						{/each}
					</div>

					<div class="code-block">
						<pre>{installCommands[activeTab]}</pre>
						<button
							class="copy-btn-small"
							type="button"
							title="Copy command"
							onclick={() => handleCopy(installCommands[activeTab])}
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

				<!-- Step 2 — Connect this workspace -->
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

	/* Tab strip — flat, bordered buttons. Active tab gets accent border + filled bg. */
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
	}
</style>
