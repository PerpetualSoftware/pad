<script lang="ts">
	import { parseFields, type Item } from '$lib/types';
	import {
		PLAYBOOK_ARGUMENT_TYPES,
		isValidInvocationSlug,
		parseArgumentsSection,
		updateArgumentsInBody,
		buildTestInvocation,
		type PlaybookArgument,
		type PlaybookArgumentType
	} from '$lib/playbooks/arguments';

	type SlugValidationState =
		| 'empty'
		| 'invalid-format'
		| 'checking'
		| 'duplicate'
		| 'ok';

	interface Props {
		wsSlug: string;
		selfItemId: string | null;
		invocationSlug: string;
		trigger: string;
		scope: string;
		status: string;
		args: PlaybookArgument[];
		bodyContent: string;
		triggers: readonly string[];
		scopes: readonly string[];
		statuses?: readonly string[];
		// Hides the status selector. Set to true on the create-form where
		// the submit buttons ("Create as Draft" / "Create as Active") own
		// the status — surfacing a select that the buttons silently
		// override is just confusing (Codex round 2 P2).
		hideStatus?: boolean;
		existingPlaybooks: Item[];
		onSlugChange: (slug: string) => void;
		onTriggerChange: (trigger: string) => void;
		onScopeChange: (scope: string) => void;
		onStatusChange: (status: string) => void;
		onArgumentsChange: (args: PlaybookArgument[]) => void;
		onBodyContentChange: (body: string) => void;
	}

	let {
		wsSlug: _wsSlug,
		selfItemId,
		invocationSlug,
		trigger,
		scope,
		status,
		args,
		bodyContent,
		triggers,
		scopes,
		statuses = ['active', 'draft', 'deprecated'],
		hideStatus = false,
		existingPlaybooks,
		onSlugChange,
		onTriggerChange,
		onScopeChange,
		onStatusChange,
		onArgumentsChange,
		onBodyContentChange
	}: Props = $props();

	// ── Slug validation state ──────────────────────────────────────────────
	// `slugValidationState` reflects the latest validation check. A short
	// debounce avoids spamming `checking` while the user is typing.
	let slugValidationState = $state<SlugValidationState>('empty');
	let duplicateTitle = $state<string | null>(null);

	// Whether the user is in "custom trigger" mode (selected Other…).
	// Initialize based on whether the current trigger is in the known list.
	let customTriggerMode = $state(false);
	let customTriggerValue = $state('');

	// Used by the slug-debounce $effect.
	let slugDebounceTimer: ReturnType<typeof setTimeout> | null = null;

	// Sync custom mode if the trigger doesn't match any known options.
	$effect(() => {
		if (trigger && triggers.length > 0 && !triggers.includes(trigger)) {
			customTriggerMode = true;
			customTriggerValue = trigger;
		}
	});

	// ── Slug validation effect ─────────────────────────────────────────────
	// Run on every change of `invocationSlug` / `existingPlaybooks` /
	// `selfItemId`. The debounce only applies to the uniqueness check —
	// format / empty checks resolve synchronously.
	$effect(() => {
		const slug = invocationSlug.trim();
		// Cancel any pending check before scheduling a fresh one.
		if (slugDebounceTimer) {
			clearTimeout(slugDebounceTimer);
			slugDebounceTimer = null;
		}
		if (!slug) {
			slugValidationState = 'empty';
			duplicateTitle = null;
			return;
		}
		if (!isValidInvocationSlug(slug)) {
			slugValidationState = 'invalid-format';
			duplicateTitle = null;
			return;
		}
		// Format is fine — start the debounced uniqueness check.
		slugValidationState = 'checking';
		slugDebounceTimer = setTimeout(() => {
			const conflict = existingPlaybooks.find((p) => {
				if (selfItemId && p.id === selfItemId) return false;
				const f = parseFields(p);
				return typeof f.invocation_slug === 'string' && f.invocation_slug === slug;
			});
			if (conflict) {
				slugValidationState = 'duplicate';
				duplicateTitle = conflict.title;
			} else {
				slugValidationState = 'ok';
				duplicateTitle = null;
			}
		}, 300);
		return () => {
			if (slugDebounceTimer) {
				clearTimeout(slugDebounceTimer);
				slugDebounceTimer = null;
			}
		};
	});

	// ── Two-way binding between structured args and body markdown ──────────
	//
	// The trick is to detect which side of the two-way binding initiated
	// the change so we don't infinite-loop. We do this by storing
	// "signatures" of the last value we emitted on each side. If the
	// incoming prop matches the signature we last emitted, the change
	// came from us — skip the work. Otherwise it came from outside and
	// we need to propagate it.

	function argsSignature(list: PlaybookArgument[]): string {
		// Use the canonical body rendering as the signature. Two arg
		// lists that produce the same body update are equivalent for
		// our purposes — the body wouldn't change anyway.
		return JSON.stringify(list);
	}

	function parsedBodySignature(body: string): string {
		return JSON.stringify(parseArgumentsSection(body));
	}

	// These trackers are seeded lazily by the effects below on first run.
	// `null` means "uninitialized" — both effects detect this and prime
	// the keys before any sync work. We avoid reading reactive props in
	// the `$state` initializer so the autofixer's
	// `state_referenced_locally` rule stays clean.
	let lastEmittedArgsKey = $state<string | null>(null);
	let lastSeenBodyKey = $state<string | null>(null);

	// Args changed externally OR locally — if it doesn't match what we
	// last emitted to the body, regenerate the body and emit.
	$effect(() => {
		const key = argsSignature(args);
		if (lastEmittedArgsKey === null) {
			// First run: prime both trackers without emitting anything.
			lastEmittedArgsKey = key;
			if (lastSeenBodyKey === null) {
				lastSeenBodyKey = parsedBodySignature(bodyContent);
			}
			return;
		}
		if (key === lastEmittedArgsKey) return;
		lastEmittedArgsKey = key;
		const newBody = updateArgumentsInBody(bodyContent, args);
		if (newBody !== bodyContent) {
			lastSeenBodyKey = parsedBodySignature(newBody);
			onBodyContentChange(newBody);
		}
	});

	// Body content changed (probably from the parent's textarea). If the
	// parsed args differ from current, emit so the structured form
	// reflects the edit.
	$effect(() => {
		const key = parsedBodySignature(bodyContent);
		if (lastSeenBodyKey === null) {
			lastSeenBodyKey = key;
			if (lastEmittedArgsKey === null) {
				lastEmittedArgsKey = argsSignature(args);
			}
			return;
		}
		if (key === lastSeenBodyKey) return;
		lastSeenBodyKey = key;
		const parsed = parseArgumentsSection(bodyContent);
		const currentKey = argsSignature(args);
		const parsedKey = argsSignature(parsed);
		if (currentKey !== parsedKey) {
			lastEmittedArgsKey = parsedKey;
			onArgumentsChange(parsed);
		}
	});

	// ── Arguments builder mutators ─────────────────────────────────────────

	function addArgument() {
		const next: PlaybookArgument[] = [
			...args,
			{ name: `arg${args.length + 1}`, type: 'string' }
		];
		onArgumentsChange(next);
	}

	function removeArgument(idx: number) {
		const next = args.filter((_, i) => i !== idx);
		onArgumentsChange(next);
	}

	function updateArgument(idx: number, patch: Partial<PlaybookArgument>) {
		const next = args.map((a, i) => (i === idx ? { ...a, ...patch } : a));
		onArgumentsChange(next);
	}

	function updateArgEnum(idx: number, raw: string) {
		const opts = raw
			.split('|')
			.map((s) => s.trim())
			.filter(Boolean);
		updateArgument(idx, { enum: opts });
	}

	function argEnumString(arg: PlaybookArgument): string {
		return arg.enum && arg.enum.length > 0 ? arg.enum.join('|') : '';
	}

	function argDefaultString(arg: PlaybookArgument): string {
		if (arg.default === undefined || arg.default === null) return '';
		if (typeof arg.default === 'boolean') return arg.default ? 'true' : 'false';
		return String(arg.default);
	}

	// ── Trigger selector handlers ──────────────────────────────────────────

	function onTriggerSelect(ev: Event) {
		const target = ev.currentTarget as HTMLSelectElement;
		const value = target.value;
		if (value === '__custom__') {
			customTriggerMode = true;
			// Keep whatever was there as the seed; emit on the custom-text input.
			return;
		}
		customTriggerMode = false;
		onTriggerChange(value);
	}

	function onCustomTriggerInput(ev: Event) {
		const target = ev.currentTarget as HTMLInputElement;
		customTriggerValue = target.value;
		onTriggerChange(target.value.trim());
	}

	// ── Test invocation rendering ──────────────────────────────────────────

	// Sample values keyed by argument name. Each input updates this map.
	let sampleValues = $state<Record<string, string>>({});

	function setSampleValue(name: string, value: string) {
		sampleValues = { ...sampleValues, [name]: value };
	}

	let invocation = $derived(buildTestInvocation(invocationSlug, args, sampleValues));

	let copiedKey = $state<string | null>(null);

	async function copyToClipboard(key: string, text: string) {
		try {
			await navigator.clipboard.writeText(text);
			copiedKey = key;
			setTimeout(() => {
				if (copiedKey === key) copiedKey = null;
			}, 1500);
		} catch {
			// Clipboard access can fail in non-secure contexts. Swallow —
			// the user can still select + copy by hand.
		}
	}

	// Helper: derive the type-specific input hint shown next to a sample input.
	function sampleHint(arg: PlaybookArgument): string {
		if (arg.type === 'flag') return '(flag — true/false)';
		if (arg.required) return '(required)';
		return '(optional)';
	}
</script>

<div class="playbook-form-fields">
	<!-- Identity row: slug + trigger -->
	<div class="form-section">
		<h3 class="section-title">Identity</h3>
		<div class="form-row">
			<label class="form-label" for="pbff-slug">Invocation slug</label>
			<input
				id="pbff-slug"
				class="form-input"
				type="text"
				value={invocationSlug}
				placeholder="ship"
				oninput={(e) => onSlugChange((e.currentTarget as HTMLInputElement).value)}
				aria-invalid={slugValidationState === 'invalid-format' ||
					slugValidationState === 'duplicate'}
				aria-describedby="pbff-slug-hint"
			/>
			<div class="slug-hint" id="pbff-slug-hint">
				{#if slugValidationState === 'empty'}
					<span class="hint-muted">Leave blank for trigger-only playbooks.</span>
				{:else if slugValidationState === 'invalid-format'}
					<span class="hint-error"
						>Slug must be lowercase letters, digits, and hyphens; 2+ chars; no leading/trailing hyphen.</span
					>
				{:else if slugValidationState === 'checking'}
					<span class="hint-muted">Checking availability…</span>
				{:else if slugValidationState === 'duplicate'}
					<span class="hint-error">Already used by {duplicateTitle ?? 'another playbook'}.</span>
				{:else if slugValidationState === 'ok'}
					<span class="hint-ok">&check; Available</span>
				{/if}
			</div>
		</div>

		<div class="form-row-pair">
			<div class="form-row">
				<label class="form-label" for="pbff-trigger">Trigger</label>
				<select
					id="pbff-trigger"
					class="form-select"
					value={customTriggerMode ? '__custom__' : trigger}
					onchange={onTriggerSelect}
				>
					{#each triggers as t (t)}
						<option value={t}>{t}</option>
					{/each}
					<option value="__custom__">Other (custom trigger)…</option>
				</select>
				{#if customTriggerMode}
					<input
						class="form-input custom-trigger"
						type="text"
						placeholder="custom-trigger-name"
						value={customTriggerValue}
						oninput={onCustomTriggerInput}
					/>
				{/if}
			</div>
			<div class="form-row">
				<label class="form-label" for="pbff-scope">Scope</label>
				<select
					id="pbff-scope"
					class="form-select"
					value={scope}
					onchange={(e) => onScopeChange((e.currentTarget as HTMLSelectElement).value)}
				>
					{#each scopes as s (s)}
						<option value={s}>{s}</option>
					{/each}
				</select>
			</div>
		</div>

		{#if !hideStatus}
			<div class="form-row">
				<label class="form-label" for="pbff-status">Status</label>
				<select
					id="pbff-status"
					class="form-select"
					value={status}
					onchange={(e) => onStatusChange((e.currentTarget as HTMLSelectElement).value)}
				>
					{#each statuses as st (st)}
						<option value={st}>{st}</option>
					{/each}
				</select>
			</div>
		{/if}
	</div>

	<!-- Arguments builder -->
	<div class="form-section">
		<h3 class="section-title">Arguments</h3>
		<p class="section-hint">
			Structured contract for the playbook's inputs. Mirrors the <code>## Arguments</code> section in
			the body — edits round-trip both ways.
		</p>

		{#if args.length === 0}
			<div class="empty-args">No arguments declared. This playbook takes no inputs.</div>
		{:else}
			<div class="args-list">
				{#each args as arg, idx (idx)}
					<div class="arg-card">
						<div class="arg-card-header">
							<div class="form-row arg-name-row">
								<label class="form-label" for="pbff-arg-name-{idx}">Name</label>
								<input
									id="pbff-arg-name-{idx}"
									class="form-input"
									type="text"
									value={arg.name}
									oninput={(e) =>
										updateArgument(idx, {
											name: (e.currentTarget as HTMLInputElement).value
										})}
								/>
							</div>
							<button
								type="button"
								class="remove-arg-btn"
								onclick={() => removeArgument(idx)}
								aria-label="Remove argument"
							>
								Remove
							</button>
						</div>
						<div class="arg-card-row">
							<div class="form-row">
								<label class="form-label" for="pbff-arg-type-{idx}">Type</label>
								<select
									id="pbff-arg-type-{idx}"
									class="form-select"
									value={arg.type}
									onchange={(e) =>
										updateArgument(idx, {
											type: (e.currentTarget as HTMLSelectElement).value as PlaybookArgumentType
										})}
								>
									{#each PLAYBOOK_ARGUMENT_TYPES as t (t)}
										<option value={t}>{t}</option>
									{/each}
								</select>
							</div>
							<div class="form-row arg-required-row">
								<label class="form-label" for="pbff-arg-req-{idx}">Required</label>
								<input
									id="pbff-arg-req-{idx}"
									type="checkbox"
									class="arg-checkbox"
									checked={arg.required ?? false}
									onchange={(e) =>
										updateArgument(idx, {
											required: (e.currentTarget as HTMLInputElement).checked
										})}
								/>
							</div>
							<div class="form-row arg-default-row">
								<label class="form-label" for="pbff-arg-default-{idx}">Default</label>
								<input
									id="pbff-arg-default-{idx}"
									class="form-input"
									type="text"
									value={argDefaultString(arg)}
									placeholder="(none)"
									oninput={(e) =>
										updateArgument(idx, {
											default: (e.currentTarget as HTMLInputElement).value
										})}
								/>
							</div>
						</div>
						<div class="form-row">
							<label class="form-label" for="pbff-arg-desc-{idx}">Description</label>
							<input
								id="pbff-arg-desc-{idx}"
								class="form-input"
								type="text"
								value={arg.description ?? ''}
								placeholder="What this argument is for"
								oninput={(e) =>
									updateArgument(idx, {
										description: (e.currentTarget as HTMLInputElement).value
									})}
							/>
						</div>
						{#if arg.type === 'enum'}
							<div class="form-row">
								<label class="form-label" for="pbff-arg-opts-{idx}">Options (pipe-separated)</label>
								<input
									id="pbff-arg-opts-{idx}"
									class="form-input"
									type="text"
									value={argEnumString(arg)}
									placeholder="squash|merge|rebase"
									oninput={(e) =>
										updateArgEnum(idx, (e.currentTarget as HTMLInputElement).value)}
								/>
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/if}

		<button type="button" class="add-arg-btn" onclick={addArgument}>+ Add argument</button>
	</div>

	<!-- Test invocation helper -->
	<div class="form-section">
		<h3 class="section-title">Test invocation</h3>
		<p class="section-hint">
			Type sample values to preview how this playbook gets invoked across surfaces.
		</p>

		{#if args.length > 0}
			<div class="sample-form">
				{#each args as arg, idx (idx)}
					<div class="form-row sample-row">
						<label class="form-label" for="pbff-sample-{idx}">
							{arg.name} <span class="sample-hint">{sampleHint(arg)}</span>
						</label>
						{#if arg.type === 'enum' && arg.enum && arg.enum.length > 0}
							<select
								id="pbff-sample-{idx}"
								class="form-select"
								value={sampleValues[arg.name] ?? ''}
								onchange={(e) =>
									setSampleValue(arg.name, (e.currentTarget as HTMLSelectElement).value)}
							>
								<option value="">(unset)</option>
								{#each arg.enum as opt (opt)}
									<option value={opt}>{opt}</option>
								{/each}
							</select>
						{:else if arg.type === 'flag'}
							<select
								id="pbff-sample-{idx}"
								class="form-select"
								value={sampleValues[arg.name] ?? 'false'}
								onchange={(e) =>
									setSampleValue(arg.name, (e.currentTarget as HTMLSelectElement).value)}
							>
								<option value="false">false</option>
								<option value="true">true</option>
							</select>
						{:else}
							<input
								id="pbff-sample-{idx}"
								class="form-input"
								type={arg.type === 'number' ? 'number' : 'text'}
								value={sampleValues[arg.name] ?? ''}
								placeholder={arg.type === 'ref' ? 'TASK-5' : ''}
								oninput={(e) =>
									setSampleValue(arg.name, (e.currentTarget as HTMLInputElement).value)}
							/>
						{/if}
					</div>
				{/each}
			</div>
		{/if}

		<div class="invocation-block">
			<div class="invocation-header">
				<span class="invocation-label">Claude Code</span>
				<button
					type="button"
					class="copy-btn"
					onclick={() => copyToClipboard('claude', invocation.claude)}
				>
					{copiedKey === 'claude' ? 'Copied!' : 'Copy'}
				</button>
			</div>
			<pre class="invocation-code">{invocation.claude}</pre>
		</div>

		<div class="invocation-block">
			<div class="invocation-header">
				<span class="invocation-label">CLI</span>
				<button
					type="button"
					class="copy-btn"
					onclick={() => copyToClipboard('cli', invocation.cli)}
				>
					{copiedKey === 'cli' ? 'Copied!' : 'Copy'}
				</button>
			</div>
			<pre class="invocation-code">{invocation.cli}</pre>
		</div>

		<div class="invocation-block">
			<div class="invocation-header">
				<span class="invocation-label">MCP (pad_playbook)</span>
				<button
					type="button"
					class="copy-btn"
					onclick={() => copyToClipboard('mcp', invocation.mcp)}
				>
					{copiedKey === 'mcp' ? 'Copied!' : 'Copy'}
				</button>
			</div>
			<pre class="invocation-code">{invocation.mcp}</pre>
		</div>
	</div>
</div>

<style>
	.playbook-form-fields {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}
	.form-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
	}
	.section-title {
		font-size: 0.9em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-secondary);
		margin: 0;
	}
	.section-hint {
		font-size: 0.85em;
		color: var(--text-muted);
		margin: 0;
	}
	.form-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.form-row-pair {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-3);
	}
	.form-label {
		font-size: 0.78em;
		font-weight: 600;
		color: var(--text-secondary);
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}
	.form-input,
	.form-select {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.92em;
	}
	.form-input:focus,
	.form-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}
	.form-input[aria-invalid='true'] {
		border-color: var(--accent-orange);
	}
	.custom-trigger {
		margin-top: var(--space-2);
	}
	.slug-hint {
		font-size: 0.8em;
		min-height: 1.2em;
		margin-top: 2px;
	}
	.hint-muted {
		color: var(--text-muted);
	}
	.hint-error {
		color: var(--accent-orange);
	}
	.hint-ok {
		color: var(--accent-green);
	}
	.empty-args {
		padding: var(--space-3);
		font-size: 0.85em;
		color: var(--text-muted);
		font-style: italic;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}
	.args-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.arg-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.arg-card-header {
		display: flex;
		align-items: flex-end;
		gap: var(--space-2);
	}
	.arg-name-row {
		flex: 1;
	}
	.arg-card-row {
		display: grid;
		grid-template-columns: 1fr auto 1fr;
		gap: var(--space-3);
		align-items: end;
	}
	.arg-required-row {
		align-items: center;
	}
	.arg-checkbox {
		width: 18px;
		height: 18px;
		accent-color: var(--accent-blue);
		cursor: pointer;
	}
	.arg-default-row {
		min-width: 0;
	}
	.remove-arg-btn {
		padding: var(--space-1) var(--space-2);
		font-size: 0.78em;
		font-weight: 600;
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: var(--accent-orange);
		border: 1px solid var(--border);
		cursor: pointer;
		white-space: nowrap;
	}
	.remove-arg-btn:hover {
		background: color-mix(in srgb, var(--accent-orange) 12%, var(--bg-secondary));
	}
	.add-arg-btn {
		align-self: flex-start;
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		font-weight: 600;
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		color: var(--text-primary);
		border: 1px dashed var(--border);
		cursor: pointer;
	}
	.add-arg-btn:hover {
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}
	.sample-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		margin-bottom: var(--space-3);
	}
	.sample-row {
		gap: var(--space-1);
	}
	.sample-hint {
		font-weight: 400;
		text-transform: none;
		letter-spacing: normal;
		color: var(--text-muted);
		font-size: 0.95em;
	}
	.invocation-block {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		margin-top: var(--space-3);
	}
	.invocation-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}
	.invocation-label {
		font-size: 0.78em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-secondary);
	}
	.copy-btn {
		padding: 2px var(--space-2);
		font-size: 0.75em;
		font-weight: 600;
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		cursor: pointer;
	}
	.copy-btn:hover {
		color: var(--accent-blue);
		border-color: var(--accent-blue);
	}
	.invocation-code {
		margin: 0;
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-family: var(--font-mono);
		font-size: 0.82em;
		line-height: 1.5;
		white-space: pre-wrap;
		word-break: break-word;
		overflow-x: auto;
	}
	code {
		font-family: var(--font-mono);
		font-size: 0.95em;
		background: var(--bg-tertiary);
		padding: 1px 4px;
		border-radius: 3px;
	}
	@media (max-width: 768px) {
		.form-row-pair {
			grid-template-columns: 1fr;
		}
		.arg-card-row {
			grid-template-columns: 1fr;
		}
	}
</style>
