<script lang="ts">
	import { authStore } from '$lib/stores/auth.svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';

	// Moving a self-hosted workspace to Pad Cloud (TASK-3612). One page for
	// both sides: the steps are the same, and a self-hosted reader is told
	// which of them happen on Cloud. It uses the existing export and import;
	// the import's own count check is step 4.

	const CLOUD_URL = 'https://app.getpad.dev';
	const CLOUD_MCP_URL = 'https://mcp.getpad.dev';
	// On Cloud the session names its own MCP address; a self-hosted reader is
	// shown Cloud's.
	const mcpUrl = $derived(authStore.cloudMode && authStore.mcpPublicUrl ? authStore.mcpPublicUrl : CLOUD_MCP_URL);
</script>

<svelte:head>
	<title>{authStore.nativeShell ? 'Move a workspace' : 'Move a workspace to Pad Cloud'} - Pad</title>
</svelte:head>

<div class="page">
	{#if authStore.nativeShell}
		<!-- The whole guide steers to Pad Cloud, which the app stores'
		     anti-steering rules forbid in a mobile app (TASK-3299; lead
		     ruling, day 92). One line: no header, no link, no steps. -->
		<p class="card">Open Pad in a browser to move a workspace.</p>
	{:else}
		<PageHeader
			title="Move a workspace to Pad Cloud"
			description="Pad Cloud runs the same Pad you host yourself. Moving a workspace makes a copy: your self-hosted workspace stays exactly as it is until you delete it."
		/>

		<section class="card" aria-labelledby="what-moves">
			<h2 id="what-moves">What comes with it</h2>
			<p>
				Collections and their schemas, items with their content and fields, comments, links,
				version history, reminders and attachments.
			</p>
			<h2>What doesn't</h2>
			<p>
				Members, pending invitations, API tokens, stars and watches. On Cloud you start as the
				only member, and you invite your team again (step 6).
			</p>
			<p>
				After the move you have two copies that don't sync. Pick one to keep working in, and
				stop editing the other.
			</p>
		</section>

		<ol class="steps">
			<li class="card">
				<h2>Export from your self-hosted Pad</h2>
				<p>
					In the workspace, open <strong>Settings → General</strong> and, next to
					<strong>Export bundle</strong>, click <strong>Download .tar.gz</strong>. Or run:
				</p>
				<pre><code>pad workspace export -o my-workspace.tar.gz</code></pre>
				<p>You need to be an owner of the workspace.</p>
			</li>

			<li class="card">
				<h2>Sign in to Pad Cloud</h2>
				{#if authStore.cloudMode}
					<p>You're on Pad Cloud already.</p>
				{:else}
					<p>
						Go to <a href={CLOUD_URL} target="_blank" rel="noopener noreferrer">app.getpad.dev</a>
						and sign up or sign in.
					</p>
				{/if}
			</li>

			<li class="card">
				<h2>Import the bundle</h2>
				<p>
					Click <strong>New Workspace</strong>, open the <strong>Import</strong> tab, and choose the <code>.tar.gz</code> file. Or,
					from a CLI signed in to Cloud:
				</p>
				<pre><code>pad workspace import my-workspace.tar.gz</code></pre>
			</li>

			<li class="card">
				<h2>Check the counts</h2>
				<p>
					When the import finishes, Pad shows how many collections, items, comments and
					attachments were in the bundle and how many arrived. They should match. If they don't,
					the message says which counts differ. Your self-hosted copy is untouched, so you can export
					and import again.
				</p>
			</li>

			<li class="card">
				<h2>Reconnect your agents</h2>
				<p>Agents and the CLI still point at your own server. Point them at Cloud:</p>
				<ul>
					<li>
						<strong>CLI:</strong> run <code>pad auth configure --mode cloud</code>, then
						<code>pad auth login</code>. Then, in each project directory, run
						<code>pad workspace link &lt;slug&gt; --url {CLOUD_URL}</code>. It moves the directory's
						link to Cloud and keeps its other settings.
					</li>
					<li>
						<strong>MCP:</strong> add <code>{mcpUrl}</code> as the remote MCP server
						(replacing your own server's URL) and sign in with OAuth.
					</li>
					<li>
						<strong>API tokens and webhooks:</strong> create them again on Cloud. Tokens from your
						server don't work there.
					</li>
				</ul>
			</li>

			<li class="card">
				<h2>Invite your team</h2>
				<p>
					Open <strong>Settings → Members</strong> and, under <strong>Invite Member</strong>, invite each
					person by email with the role they had before.
				</p>
			</li>
		</ol>
	{/if}
</div>

<style>
	.page {
		max-width: 760px;
		display: flex;
		flex-direction: column;
		gap: var(--space-4, 1rem);
	}
	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg, 8px);
		padding: var(--space-4, 1rem) var(--space-5, 1.25rem);
	}
	h2 {
		font-size: 1rem;
		font-weight: 600;
		margin: 0 0 var(--space-2, 0.5rem);
		color: var(--text-primary);
	}
	p,
	li {
		color: var(--text-secondary);
		line-height: 1.55;
	}
	p {
		margin: 0 0 var(--space-2, 0.5rem);
	}
	/* A link inside running text must not rely on colour alone (axe link-in-text-block). */
	p a {
		text-decoration: underline;
	}
	p:last-child {
		margin-bottom: 0;
	}
	.steps {
		list-style: none;
		counter-reset: step;
		padding: 0;
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-3, 0.75rem);
	}
	.steps > li {
		counter-increment: step;
	}
	.steps > li > h2::before {
		content: counter(step) '. ';
		color: var(--text-muted);
	}
	ul {
		margin: 0;
		padding-left: 1.25rem;
	}
	pre {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius, 6px);
		padding: var(--space-2, 0.5rem) var(--space-3, 0.75rem);
		overflow-x: auto;
		margin: 0 0 var(--space-2, 0.5rem);
	}
	code {
		font-family: var(--font-mono, monospace);
		font-size: 0.9em;
	}
</style>
