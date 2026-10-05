<script lang="ts">
	import Button from '$lib/components/common/Button.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import type { AppInstallCode } from '$lib/types';

	/**
	 * The install-code handoff (SPEC-6 U9a, TASK-3413). The code is shown
	 * exactly once: the server never returns it again, so this panel is the
	 * owner's only chance to copy it. Its parent drops the code from state when
	 * the panel is dismissed.
	 */
	interface Props {
		code: AppInstallCode;
		appName: string;
		ondone: () => void;
	}
	let { code, appName, ondone }: Props = $props();

	let copied = $state(false);

	async function copy() {
		copied = await copyToClipboard(code.install_code);
	}

	let expiresLabel = $derived.by(() => {
		const d = new Date(code.expires_at);
		return Number.isNaN(d.getTime()) ? code.expires_at : d.toLocaleTimeString();
	});
</script>

<div class="code-panel" role="region" aria-label="Install code for {appName}">
	<h3>Give this code to {appName}</h3>
	<p class="notice">{code.notice}</p>
	<div class="code-row">
		<code class="code" data-testid="app-install-code">{code.install_code}</code>
		<Button size="sm" onclick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
	</div>
	<p class="expires">Expires at {expiresLabel}. It will not be shown again.</p>
	<Button variant="primary" size="sm" onclick={ondone}>Done</Button>
</div>

<style>
	.code-panel {
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius);
		padding: var(--space-4);
		background: var(--bg-secondary);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		align-items: flex-start;
	}
	h3 {
		margin: 0;
		font-size: 1em;
	}
	.notice,
	.expires {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-secondary);
	}
	.code-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		flex-wrap: wrap;
		max-width: 100%;
	}
	.code {
		font-family: var(--font-mono, monospace);
		font-size: 0.95em;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		word-break: break-all;
		user-select: all;
	}
</style>
