<script lang="ts">
	let {
		content = '',
		onUpdate,
	}: {
		content?: string;
		onUpdate?: (markdown: string) => void;
	} = $props();

	let localContent = $state('');
	let textarea = $state<HTMLTextAreaElement>();

	// Sync when content prop changes (doc switch or mode toggle)
	$effect(() => {
		localContent = content;
		// Auto-size after content sync
		requestAnimationFrame(autoSize);
	});

	function autoSize() {
		if (!textarea) return;
		textarea.style.height = 'auto';
		textarea.style.height = textarea.scrollHeight + 'px';
	}

	function handleInput(e: Event) {
		const target = e.target as HTMLTextAreaElement;
		localContent = target.value;
		onUpdate?.(localContent);
		autoSize();
	}
</script>

<textarea
	bind:this={textarea}
	value={localContent}
	oninput={handleInput}
	class="raw-textarea"
	spellcheck="false"
></textarea>

<style>
	.raw-textarea {
		width: 100%;
		min-height: 200px;
		resize: none;
		background: var(--bg-tertiary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		font-family: var(--font-mono);
		font-size: 0.9em;
		line-height: 1.6;
		tab-size: 2;
		white-space: pre-wrap;
		word-wrap: break-word;
		margin: 0;
		overflow: hidden;
	}
	.raw-textarea:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* Print: let the raw markdown source flow naturally — no textarea
	   box, no fixed height, monospace plain text so the source is at
	   least legible on paper when a user prints while editing in
	   Markdown mode (PLAN-620 / TASK-622). */
	@media print {
		.raw-textarea {
			border: none !important;
			background: transparent !important;
			color: #000 !important;
			padding: 0 !important;
			margin: 0 !important;
			min-height: 0 !important;
			height: auto !important;
			overflow: visible !important;
			resize: none !important;
			outline: none !important;
			box-shadow: none !important;
			width: 100% !important;
			font-family: var(--font-mono) !important;
			font-size: 10pt !important;
			line-height: 1.55 !important;
			white-space: pre-wrap !important;
			word-wrap: break-word;
			page-break-inside: auto;
		}
	}
</style>
