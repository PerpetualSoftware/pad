<script lang="ts">
	import { copyToClipboard } from '$lib/utils/clipboard';

	interface Props {
		/** Workspace slug — used to link "View IDEA-1" into the right URL. */
		wsSlug: string;
		/** Workspace owner username — used to build the IDEA-1 deep link. */
		username: string;
		/** Slug of the seeded IDEA-1 (the workspace bootstrap creates it
		 *  with this slug). Defaults to the canonical seed slug; passing
		 *  it explicitly keeps the component decoupled from the seeder. */
		ideaSlug?: string;
	}

	let { wsSlug, username, ideaSlug = 'welcome-lets-get-this-place-set-up' }: Props = $props();

	const TRIGGER_PHRASE = 'use pad to get IDEA-1';

	let copied = $state(false);

	async function copyTrigger() {
		const ok = await copyToClipboard(TRIGGER_PHRASE);
		if (ok) {
			copied = true;
			setTimeout(() => {
				copied = false;
			}, 1500);
		}
	}
</script>

<div class="idea-banner">
	<div class="idea-banner-icon" aria-hidden="true">💡</div>
	<div class="idea-banner-body">
		<h2>Your workspace has an idea waiting.</h2>
		<p>
			Open a fresh agent session — Claude Code, Cursor, Codex, whatever you have —
			and say:
		</p>
		<div class="trigger-row">
			<code class="trigger-phrase">{TRIGGER_PHRASE}</code>
			<button class="copy-btn" type="button" onclick={copyTrigger} title="Copy to clipboard">
				{copied ? 'Copied!' : 'Copy'}
			</button>
		</div>
		<p class="idea-banner-footnote">
			IDEA-1 is a note from your future self to whoever's helping you set up. The
			agent will read it and walk through your project with you, capturing what
			you tell it as plans, tasks, and ideas — using your real work, not toy data.
			<a href="/{username}/{wsSlug}/ideas/{ideaSlug}">Read it first</a> if you'd
			like to see what's there.
		</p>
	</div>
</div>

<style>
	.idea-banner {
		display: flex;
		gap: var(--space-4);
		align-items: flex-start;
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-secondary));
		border: 1px solid color-mix(in srgb, var(--accent-blue) 30%, var(--border));
		border-radius: var(--radius);
		padding: var(--space-5);
		max-width: 600px;
	}

	.idea-banner-icon {
		font-size: 1.6em;
		line-height: 1;
		margin-top: 2px;
		flex-shrink: 0;
	}

	.idea-banner-body {
		flex: 1;
		min-width: 0;
	}

	.idea-banner-body h2 {
		font-size: 1.05em;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0 0 var(--space-2) 0;
	}

	.idea-banner-body p {
		font-size: 0.92em;
		color: var(--text-secondary);
		margin: 0 0 var(--space-3) 0;
		line-height: 1.5;
	}

	.trigger-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
		flex-wrap: wrap;
	}

	.trigger-phrase {
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
		font-size: 0.95em;
		color: var(--text-primary);
		font-family: var(--font-mono, monospace);
		user-select: all;
	}

	.copy-btn {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		color: var(--text-primary);
		font-size: 0.85em;
		cursor: pointer;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		white-space: nowrap;
	}

	.copy-btn:hover {
		background: var(--bg-primary);
		border-color: var(--accent-blue);
	}

	.idea-banner-footnote {
		font-size: 0.85em !important;
		color: var(--text-muted) !important;
		margin: 0 !important;
		line-height: 1.5;
	}

	.idea-banner-footnote a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.idea-banner-footnote a:hover {
		text-decoration: underline;
	}
</style>
