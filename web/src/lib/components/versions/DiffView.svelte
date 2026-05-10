<script lang="ts">
	import { diffLines, type Change } from 'diff';
	import { SvelteSet } from 'svelte/reactivity';

	interface Props {
		oldContent: string;
		newContent: string;
		oldLabel?: string;
		newLabel?: string;
	}

	let {
		oldContent,
		newContent,
		oldLabel = 'Previous',
		newLabel = 'Current'
	}: Props = $props();

	const CONTEXT_LINES = 3;

	interface DiffLine {
		type: 'added' | 'removed' | 'unchanged';
		text: string;
		oldLineNum: number | null;
		newLineNum: number | null;
	}

	type MidEntry =
		| { kind: 'line'; line: DiffLine }
		| { kind: 'htmlBlock'; lines: DiffLine[]; added: number; removed: number; unchanged: number };

	interface DisplayEntry {
		kind: 'line' | 'separator' | 'htmlBlock';
		line?: DiffLine;
		skipped?: number;
		lines?: DiffLine[];
		added?: number;
		removed?: number;
		unchanged?: number;
	}

	/**
	 * Expand diff changes into individual DiffLine entries,
	 * tracking line numbers for both old and new content.
	 */
	function buildDiffLines(changes: Change[]): DiffLine[] {
		const lines: DiffLine[] = [];
		let oldNum = 1;
		let newNum = 1;

		for (const change of changes) {
			const lineTexts = change.value.replace(/\n$/, '').split('\n');

			// Handle empty string change (can happen with trailing newlines)
			if (change.value === '') continue;

			for (const text of lineTexts) {
				if (change.added) {
					lines.push({ type: 'added', text, oldLineNum: null, newLineNum: newNum });
					newNum++;
				} else if (change.removed) {
					lines.push({ type: 'removed', text, oldLineNum: oldNum, newLineNum: null });
					oldNum++;
				} else {
					lines.push({ type: 'unchanged', text, oldLineNum: oldNum, newLineNum: newNum });
					oldNum++;
					newNum++;
				}
			}
		}

		return lines;
	}

	/**
	 * Group runs of `\`\`\`html` ... `\`\`\`` lines into a single htmlBlock chunk
	 * when they contain any added/removed line. Entirely-unchanged blocks are
	 * passed through as individual lines so the standard context-collapse can
	 * handle them. Unbalanced fences degrade to plain lines.
	 */
	function chunkHtmlBlocks(lines: DiffLine[]): MidEntry[] {
		const entries: MidEntry[] = [];
		const fenceOpenRe = /^(`{3,})html$/;
		let i = 0;

		while (i < lines.length) {
			const match = lines[i].text.match(fenceOpenRe);
			if (match) {
				const fence = match[1];
				// Search forward for matching close fence (exact backtick run, no language)
				let close = -1;
				for (let j = i + 1; j < lines.length; j++) {
					if (lines[j].text === fence) {
						close = j;
						break;
					}
				}

				if (close !== -1) {
					const blockLines = lines.slice(i, close + 1);
					let added = 0;
					let removed = 0;
					let unchanged = 0;
					for (const line of blockLines) {
						if (line.type === 'added') added++;
						else if (line.type === 'removed') removed++;
						else unchanged++;
					}

					if (added > 0 || removed > 0) {
						entries.push({ kind: 'htmlBlock', lines: blockLines, added, removed, unchanged });
					} else {
						// Entirely unchanged — let context-collapse handle them
						for (const line of blockLines) {
							entries.push({ kind: 'line', line });
						}
					}
					i = close + 1;
					continue;
				}
				// Unbalanced fence — fall through to plain line
			}

			entries.push({ kind: 'line', line: lines[i] });
			i++;
		}

		return entries;
	}

	/**
	 * Collapse unchanged lines outside the context window into
	 * separator entries showing how many lines were skipped.
	 */
	function collapseContext(entries: MidEntry[]): DisplayEntry[] {
		// Find indices of all "change" entries (htmlBlock chunks or non-unchanged lines)
		const changedIndices = new Set<number>();
		for (let i = 0; i < entries.length; i++) {
			const e = entries[i];
			if (e.kind === 'htmlBlock' || (e.kind === 'line' && e.line.type !== 'unchanged')) {
				changedIndices.add(i);
			}
		}

		// If there are no changes at all, count total raw lines and show a single message
		if (changedIndices.size === 0) {
			let total = 0;
			for (const e of entries) {
				if (e.kind === 'line') total++;
			}
			return [{ kind: 'separator', skipped: total }];
		}

		// Mark which entries should be visible (within CONTEXT_LINES of a change)
		const visible = new Set<number>();
		for (const idx of changedIndices) {
			for (let offset = -CONTEXT_LINES; offset <= CONTEXT_LINES; offset++) {
				const target = idx + offset;
				if (target >= 0 && target < entries.length) {
					visible.add(target);
				}
			}
		}

		const out: DisplayEntry[] = [];
		let i = 0;
		while (i < entries.length) {
			if (visible.has(i)) {
				const e = entries[i];
				if (e.kind === 'htmlBlock') {
					out.push({
						kind: 'htmlBlock',
						lines: e.lines,
						added: e.added,
						removed: e.removed,
						unchanged: e.unchanged
					});
				} else {
					out.push({ kind: 'line', line: e.line });
				}
				i++;
			} else {
				// Count consecutive hidden entries — only `line` entries can be hidden
				// because htmlBlock entries are always changes (and thus visible).
				let skippedCount = 0;
				while (i < entries.length && !visible.has(i)) {
					if (entries[i].kind === 'line') skippedCount++;
					i++;
				}
				out.push({ kind: 'separator', skipped: skippedCount });
			}
		}

		return out;
	}

	let changes = $derived(diffLines(oldContent, newContent));
	let diffLineEntries = $derived(buildDiffLines(changes));
	let chunkedEntries = $derived(chunkHtmlBlocks(diffLineEntries));
	let displayEntries = $derived(collapseContext(chunkedEntries));

	const expandedBlocks = new SvelteSet<number>();

	function toggleBlock(idx: number) {
		if (expandedBlocks.has(idx)) expandedBlocks.delete(idx);
		else expandedBlocks.add(idx);
	}

	let stats = $derived.by(() => {
		let added = 0;
		let removed = 0;
		for (const line of diffLineEntries) {
			if (line.type === 'added') added++;
			if (line.type === 'removed') removed++;
		}
		return { added, removed };
	});
</script>

<div class="diff-view">
	<div class="diff-header">
		<div class="diff-labels">
			<span class="label label-old">{oldLabel}</span>
			<span class="arrow">&rarr;</span>
			<span class="label label-new">{newLabel}</span>
		</div>
		<div class="diff-stats">
			{#if stats.added > 0}
				<span class="stat stat-added">+{stats.added}</span>
			{/if}
			{#if stats.removed > 0}
				<span class="stat stat-removed">-{stats.removed}</span>
			{/if}
			{#if stats.added === 0 && stats.removed === 0}
				<span class="stat stat-none">No changes</span>
			{/if}
		</div>
	</div>

	<div class="diff-body">
		{#each displayEntries as entry, idx (idx)}
			{#if entry.kind === 'separator'}
				<div class="separator">
					<span class="separator-text">... {entry.skipped} unchanged {entry.skipped === 1 ? 'line' : 'lines'} ...</span>
				</div>
			{:else if entry.kind === 'htmlBlock' && entry.lines}
				<div class="html-block-chunk">
					<button
						type="button"
						class="html-block-chunk-summary"
						onclick={() => toggleBlock(idx)}
						aria-expanded={expandedBlocks.has(idx)}
					>
						<span class="html-block-chunk-caret" class:expanded={expandedBlocks.has(idx)}>&#9654;</span>
						<span>HTML block changed</span>
						<span class="html-block-chunk-stats">
							{#if (entry.added ?? 0) > 0}
								<span class="html-block-chunk-stat-added">+{entry.added}</span>
							{/if}
							{#if (entry.removed ?? 0) > 0}
								<span class="html-block-chunk-stat-removed">-{entry.removed}</span>
							{/if}
							({entry.unchanged ?? 0} unchanged)
						</span>
						<span class="html-block-chunk-stats">— click to {expandedBlocks.has(idx) ? 'collapse' : 'expand'}</span>
					</button>
					{#if expandedBlocks.has(idx)}
						<div class="html-block-chunk-lines">
							{#each entry.lines as innerLine (`${idx}-${innerLine.oldLineNum ?? 'n'}-${innerLine.newLineNum ?? 'n'}-${innerLine.type}`)}
								<div class="diff-line {innerLine.type}">
									<span class="line-num old-num">{innerLine.oldLineNum ?? ''}</span>
									<span class="line-num new-num">{innerLine.newLineNum ?? ''}</span>
									<span class="line-prefix">{innerLine.type === 'added' ? '+' : innerLine.type === 'removed' ? '-' : ' '}</span>
									<span class="line-content">{innerLine.text}</span>
								</div>
							{/each}
						</div>
					{/if}
				</div>
			{:else if entry.line}
				<div class="diff-line {entry.line.type}">
					<span class="line-num old-num">{entry.line.oldLineNum ?? ''}</span>
					<span class="line-num new-num">{entry.line.newLineNum ?? ''}</span>
					<span class="line-prefix">{entry.line.type === 'added' ? '+' : entry.line.type === 'removed' ? '-' : ' '}</span>
					<span class="line-content">{entry.line.text}</span>
				</div>
			{/if}
		{/each}
	</div>
</div>

<style>
	.diff-view {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
		font-family: var(--font-mono);
		font-size: 0.85em;
		line-height: 1.5;
	}

	.diff-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border-bottom: 1px solid var(--border);
		font-family: var(--font-ui);
	}

	.diff-labels {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.label {
		font-size: 0.85em;
		font-weight: 500;
	}

	.label-old {
		color: var(--accent-red, #ef4444);
	}

	.label-new {
		color: var(--accent-green);
	}

	.arrow {
		color: var(--text-muted);
		font-size: 0.85em;
	}

	.diff-stats {
		display: flex;
		gap: var(--space-2);
	}

	.stat {
		font-size: 0.8em;
		font-weight: 600;
		padding: 1px var(--space-2);
		border-radius: var(--radius-sm);
	}

	.stat-added {
		color: var(--accent-green);
		background: color-mix(in srgb, var(--accent-green) 12%, transparent);
	}

	.stat-removed {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
	}

	.stat-none {
		color: var(--text-muted);
	}

	.diff-body {
		overflow-x: auto;
	}

	.diff-line {
		display: flex;
		align-items: stretch;
		min-height: 1.5em;
		white-space: pre;
	}

	.diff-line.unchanged {
		background: var(--bg-primary, #1a1a1a);
	}

	.diff-line.unchanged .line-content,
	.diff-line.unchanged .line-prefix {
		color: var(--text-muted);
	}

	.diff-line.added {
		background: color-mix(in srgb, var(--accent-green) 10%, var(--bg-primary, #1a1a1a));
	}

	.diff-line.added .line-content,
	.diff-line.added .line-prefix {
		color: var(--accent-green);
	}

	.diff-line.removed {
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, var(--bg-primary, #1a1a1a));
	}

	.diff-line.removed .line-content,
	.diff-line.removed .line-prefix {
		color: var(--accent-red, #ef4444);
	}

	.line-num {
		display: inline-block;
		width: 3.5em;
		padding: 0 var(--space-2);
		text-align: right;
		color: var(--text-muted);
		background: var(--bg-secondary);
		border-right: 1px solid var(--border);
		user-select: none;
		flex-shrink: 0;
	}

	.line-prefix {
		display: inline-block;
		width: 1.5em;
		text-align: center;
		flex-shrink: 0;
		user-select: none;
	}

	.line-content {
		flex: 1;
		padding-right: var(--space-3);
	}

	.separator {
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-1) 0;
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
		border-bottom: 1px solid var(--border);
	}

	.separator-text {
		font-size: 0.8em;
		color: var(--text-muted);
		font-family: var(--font-ui);
	}

	.html-block-chunk {
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
		border-bottom: 1px solid var(--border);
	}
	.html-block-chunk-summary {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		cursor: pointer;
		color: var(--text-secondary);
		font-family: var(--font-ui);
		font-size: 0.85em;
		background: transparent;
		border: none;
		width: 100%;
		text-align: left;
	}
	.html-block-chunk-summary:hover {
		background: var(--bg-tertiary);
	}
	.html-block-chunk-caret {
		display: inline-block;
		width: 0.8em;
		color: var(--text-muted);
		transition: transform 0.12s;
	}
	.html-block-chunk-caret.expanded {
		transform: rotate(90deg);
	}
	.html-block-chunk-stats {
		color: var(--text-muted);
		font-size: 0.95em;
	}
	.html-block-chunk-stat-added {
		color: var(--accent-green);
		font-weight: 600;
	}
	.html-block-chunk-stat-removed {
		color: var(--accent-red, #ef4444);
		font-weight: 600;
	}
	.html-block-chunk-lines {
		border-top: 1px solid var(--border);
	}
</style>
