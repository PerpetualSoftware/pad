<script lang="ts">
	import { diffLines, type Change } from 'diff';

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

	interface DisplayEntry {
		kind: 'line' | 'separator';
		line?: DiffLine;
		skipped?: number;
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
	 * Collapse unchanged lines outside the context window into
	 * separator entries showing how many lines were skipped.
	 */
	function collapseContext(lines: DiffLine[]): DisplayEntry[] {
		// Find indices of all changed lines
		const changedIndices = new Set<number>();
		for (let i = 0; i < lines.length; i++) {
			if (lines[i].type !== 'unchanged') {
				changedIndices.add(i);
			}
		}

		// If there are no changes at all, show a short message
		if (changedIndices.size === 0) {
			return [{ kind: 'separator', skipped: lines.length }];
		}

		// Mark which lines should be visible (within CONTEXT_LINES of a change)
		const visible = new Set<number>();
		for (const idx of changedIndices) {
			for (let offset = -CONTEXT_LINES; offset <= CONTEXT_LINES; offset++) {
				const target = idx + offset;
				if (target >= 0 && target < lines.length) {
					visible.add(target);
				}
			}
		}

		const entries: DisplayEntry[] = [];
		let i = 0;
		while (i < lines.length) {
			if (visible.has(i)) {
				entries.push({ kind: 'line', line: lines[i] });
				i++;
			} else {
				// Count consecutive hidden lines
				let skippedCount = 0;
				while (i < lines.length && !visible.has(i)) {
					skippedCount++;
					i++;
				}
				entries.push({ kind: 'separator', skipped: skippedCount });
			}
		}

		return entries;
	}

	let changes = $derived(diffLines(oldContent, newContent));
	let diffLineEntries = $derived(buildDiffLines(changes));
	let displayEntries = $derived(collapseContext(diffLineEntries));

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
		{#each displayEntries as entry (entry.kind === 'separator' ? `sep-${displayEntries.indexOf(entry)}` : `line-${entry.line?.oldLineNum ?? 'n'}-${entry.line?.newLineNum ?? 'n'}-${entry.line?.type}`)}
			{#if entry.kind === 'separator'}
				<div class="separator">
					<span class="separator-text">... {entry.skipped} unchanged {entry.skipped === 1 ? 'line' : 'lines'} ...</span>
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
</style>
