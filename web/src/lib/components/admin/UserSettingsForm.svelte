<!--
  Settings & overrides — the lifted form that fills the modal's Settings
  tab. PLAN-1542 / TASK-1551.

  Per the plan this is a PARALLEL implementation of the inline-expand
  block in admin/+page.svelte; the inline expand is intentionally left
  alone in this PR so behaviour parity can be verified side-by-side
  before T1555 deletes it. The shared helpers (parsePlanOverrides,
  parseStorageInput, formatStorageBytes) are duplicated here for now to
  keep the lift self-contained — they collapse back to one home in
  T1555 when the inline-expand goes away.

  Behaviour parity required: role change w/ promote-demote confirm,
  admin password reset (email vs temp-password result handling), account
  disable/enable (with NEW typed-email confirmation for the destructive
  side), plan selector, plan-overrides grid, storage override with
  parse/preview/reset. Workspaces list moves to the Workspaces tab
  (T1552) — not here.
-->
<script lang="ts">
	import { adminFetch, adminPatch, adminPost, type AdminUser } from '$lib/stores/admin.svelte';

	interface Props {
		user: AdminUser;
		/** Called with the freshly-refetched user after any successful save
		 *  so the parent can keep its list-row in sync. */
		onUserUpdated?: (updated: AdminUser) => void;
	}

	let { user, onUserUpdated }: Props = $props();

	// --- Plan override field metadata. storage_bytes has its own dedicated
	// input (parses shorthand), so it's omitted here.
	const overrideFields = [
		{ key: 'workspaces', label: 'Workspaces', hint: 'Max workspaces owned' },
		{ key: 'items_per_workspace', label: 'Items per workspace', hint: 'Max items in each workspace' },
		{ key: 'members_per_workspace', label: 'Members per workspace', hint: 'Max members per workspace' },
		{ key: 'api_tokens', label: 'API tokens', hint: 'Max API tokens' },
		{ key: 'webhooks', label: 'Webhooks', hint: 'Max webhooks per workspace' }
	];
	const overrideFieldKeys = new Set([...overrideFields.map((f) => f.key), 'storage_bytes']);

	// --- State (mirrors the inline-expand block 1:1)
	let editPlan = $state('free');
	let editRole = $state('member');
	let editOverrides = $state<Record<string, string>>({});
	let editStorageOverride = $state('');
	let storageOverrideError = $state('');
	let extraOverrides = $state<Record<string, number>>({});
	let saving = $state(false);
	let saveMsg = $state('');

	let roleConfirm = $state(false);
	let roleSaving = $state(false);
	let roleMsg = $state('');

	let resetConfirm = $state(false);
	let resetSaving = $state(false);
	let resetResult = $state<{ method: string; temp_password?: string; message: string } | null>(null);
	let resetError = $state('');

	let disableConfirm = $state(false);
	let disableSaving = $state(false);
	let disableMsg = $state('');
	// NEW for T1551: typed-email confirmation for disable. The user must
	// type the target email exactly before the Disable button enables.
	let disableTyped = $state('');

	// Hydrate state from the user prop whenever it changes (modal reopened
	// on a different row, or parent re-fetched after a save).
	$effect(() => {
		editPlan = user.plan || 'free';
		editRole = user.role || 'member';
		const ov = parsePlanOverrides(user.plan_overrides);
		editOverrides = {};
		extraOverrides = {};
		for (const f of overrideFields) {
			editOverrides[f.key] = f.key in ov ? String(ov[f.key]) : '';
		}
		editStorageOverride = 'storage_bytes' in ov ? formatStorageBytes(ov.storage_bytes) : '';
		storageOverrideError = '';
		for (const [k, v] of Object.entries(ov)) {
			if (!overrideFieldKeys.has(k)) extraOverrides[k] = v;
		}
		saveMsg = '';
		roleConfirm = false;
		roleMsg = '';
		resetConfirm = false;
		resetResult = null;
		resetError = '';
		disableConfirm = false;
		disableTyped = '';
		disableMsg = '';
	});

	function parsePlanOverrides(raw: unknown): Record<string, number> {
		if (raw == null || raw === '') return {};
		if (typeof raw === 'object') return raw as Record<string, number>;
		if (typeof raw !== 'string') return {};
		try {
			const parsed = JSON.parse(raw);
			if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
				return parsed as Record<string, number>;
			}
		} catch {
			/* fall through */
		}
		return {};
	}

	function parseStorageInput(input: string): number | null {
		const trimmed = input.trim();
		if (trimmed === '-1') return -1;
		if (/^\d+$/.test(trimmed)) {
			const n = Number(trimmed);
			return Number.isSafeInteger(n) && n >= 0 ? n : null;
		}
		const m = trimmed.match(/^(\d+(?:\.\d+)?)\s*([KMGT]?B?)$/i);
		if (!m) return null;
		const value = parseFloat(m[1]);
		if (isNaN(value) || value < 0) return null;
		const unit = m[2].toUpperCase().replace(/B$/, '');
		const mult: Record<string, number> = {
			'': 1,
			K: 1024,
			M: 1024 * 1024,
			G: 1024 * 1024 * 1024,
			T: 1024 * 1024 * 1024 * 1024
		};
		const factor = mult[unit];
		if (factor === undefined) return null;
		const bytes = Math.round(value * factor);
		return Number.isSafeInteger(bytes) ? bytes : null;
	}

	function formatStorageBytes(n: number): string {
		if (n === -1) return '-1';
		if (n < 0) return String(n);
		const KB = 1024;
		const MB = KB * 1024;
		const GB = MB * 1024;
		const TB = GB * 1024;
		if (n >= TB && n % TB === 0) return `${n / TB} TB`;
		if (n >= GB && n % GB === 0) return `${n / GB} GB`;
		if (n >= MB && n % MB === 0) return `${n / MB} MB`;
		if (n >= KB && n % KB === 0) return `${n / KB} KB`;
		return String(n);
	}

	let storageOverridePreview = $derived.by(() => {
		const trimmed = editStorageOverride.trim();
		if (trimmed === '') return '';
		const parsed = parseStorageInput(trimmed);
		if (parsed === null) return 'Invalid input';
		if (parsed === -1) return 'Unlimited';
		return `= ${formatStorageBytes(parsed)} (${parsed.toLocaleString()} bytes)`;
	});

	function roleAction(): string {
		return editRole === 'admin' ? 'Promote' : 'Demote';
	}

	// All async handlers snapshot user.id (and the target email for the
	// typed-confirm gate) at entry so a mid-flight modal swap to a
	// different user can't fetch/mutate the wrong row (Codex review on
	// PR #606). Mirrors the inline-expand handlers' pattern.
	async function changeRole() {
		const userId = user.id;
		roleSaving = true;
		roleMsg = '';
		try {
			await adminPatch(`/admin/users/${userId}`, { role: editRole });
			const updated = await adminFetch(`/admin/users/${userId}`);
			onUserUpdated?.(updated as AdminUser);
			roleMsg = 'Role updated';
			roleConfirm = false;
		} catch (e) {
			roleMsg = e instanceof Error ? e.message : 'Role change failed';
		} finally {
			roleSaving = false;
		}
	}

	async function resetPassword() {
		const userId = user.id;
		resetSaving = true;
		resetError = '';
		resetResult = null;
		try {
			const result = await adminPost(`/admin/users/${userId}/reset-password`);
			resetResult = result;
		} catch (e) {
			resetError = e instanceof Error ? e.message : 'Password reset failed';
		} finally {
			resetSaving = false;
			resetConfirm = false;
		}
	}

	async function toggleDisable() {
		const userId = user.id;
		const targetEmail = user.email;
		const wasDisabled = !!user.disabled_at;
		// Typed-email confirmation gate on the destructive side only. The
		// comparison trims whitespace because paste from various sources
		// (terminal, table cells, mail clients) often picks up surrounding
		// space; the UI copy already says "paste tolerant" implicitly via
		// the placeholder.
		if (!wasDisabled && disableTyped.trim() !== targetEmail) {
			disableMsg = 'Type the user’s email to confirm (paste tolerant).';
			return;
		}
		disableSaving = true;
		disableMsg = '';
		try {
			const action = wasDisabled ? 'enable' : 'disable';
			await adminPost(`/admin/users/${userId}/${action}`);
			const updated = await adminFetch(`/admin/users/${userId}`);
			onUserUpdated?.(updated as AdminUser);
			disableMsg = wasDisabled ? 'User re-enabled' : 'User disabled';
			disableConfirm = false;
			disableTyped = '';
		} catch (e) {
			disableMsg = e instanceof Error ? e.message : 'Action failed';
		} finally {
			disableSaving = false;
		}
	}

	async function saveUser() {
		const userId = user.id;
		saving = true;
		saveMsg = '';
		storageOverrideError = '';
		try {
			const overrides: Record<string, number> = { ...extraOverrides };
			for (const f of overrideFields) {
				const val = editOverrides[f.key]?.trim();
				if (val !== '' && val !== undefined) {
					const num = Number(val);
					if (isNaN(num) || !Number.isInteger(num)) {
						saveMsg = `"${f.label}" must be a whole number`;
						saving = false;
						return;
					}
					overrides[f.key] = num;
				}
			}
			const rawStorage = editStorageOverride.trim();
			if (rawStorage !== '') {
				const parsed = parseStorageInput(rawStorage);
				if (parsed === null) {
					storageOverrideError =
						'Storage override must be bytes (1024), shorthand (500 MB / 10 GB), or -1 for unlimited';
					saveMsg = '';
					saving = false;
					return;
				}
				overrides['storage_bytes'] = parsed;
			}
			const overridesJSON =
				Object.keys(overrides).length > 0 ? JSON.stringify(overrides) : '';
			await adminPatch(`/admin/users/${userId}`, {
				plan: editPlan,
				plan_overrides: overridesJSON
			});
			const updated = await adminFetch(`/admin/users/${userId}`);
			onUserUpdated?.(updated as AdminUser);
			saveMsg = 'Saved';
		} catch (e) {
			saveMsg = e instanceof Error ? e.message : 'Save failed';
		} finally {
			saving = false;
		}
	}
</script>

<div class="settings-form">
	<!-- Role -->
	<div class="field">
		<label class="field-label" for="settings-role">Role</label>
		<div class="role-row">
			<select id="settings-role" bind:value={editRole}>
				<option value="member">member</option>
				<option value="admin">admin</option>
			</select>
			{#if editRole !== (user.role || 'member')}
				{#if !roleConfirm}
					<button
						class="btn"
						type="button"
						onclick={() => {
							roleConfirm = true;
							roleMsg = '';
						}}>Change Role</button
					>
				{:else}
					<div class="role-confirm">
						<span class="role-confirm-msg">
							{roleAction()} <strong>{user.name || user.username}</strong> to {editRole}?
						</span>
						<button
							class="btn btn-primary"
							type="button"
							disabled={roleSaving}
							onclick={changeRole}>{roleSaving ? 'Saving…' : 'Confirm'}</button
						>
						<button
							class="btn btn-sub"
							type="button"
							onclick={() => {
								roleConfirm = false;
								editRole = user.role || 'member';
							}}>Cancel</button
						>
					</div>
				{/if}
			{/if}
		</div>
		{#if roleMsg}<div class="msg">{roleMsg}</div>{/if}
	</div>

	<!-- Password reset -->
	<div class="field">
		<div class="field-label">Password reset</div>
		{#if resetResult}
			<div class="msg ok">{resetResult.message}</div>
			{#if resetResult.method === 'temporary_password' && resetResult.temp_password}
				<div class="temp-password">
					Temporary password:
					<code>{resetResult.temp_password}</code>
				</div>
				<p class="hint">Share this with the user via a secure channel; it expires on first login.</p>
			{/if}
		{:else if resetError}
			<div class="msg error">{resetError}</div>
		{/if}
		{#if !resetConfirm}
			<button class="btn" type="button" onclick={() => (resetConfirm = true)}>Reset password</button>
		{:else}
			<div class="row">
				<span>Send a password-reset email or generate a temporary password?</span>
				<button
					class="btn btn-primary"
					type="button"
					disabled={resetSaving}
					onclick={resetPassword}>{resetSaving ? 'Working…' : 'Confirm'}</button
				>
				<button class="btn btn-sub" type="button" onclick={() => (resetConfirm = false)}>Cancel</button>
			</div>
		{/if}
	</div>

	<!-- Disable / enable -->
	<div class="field">
		<div class="field-label">Account status</div>
		{#if disableMsg}<div class="msg" class:error={disableMsg.includes('failed') || disableMsg.includes('Type the')}>{disableMsg}</div>{/if}
		{#if !user.disabled_at}
			{#if !disableConfirm}
				<button
					class="btn btn-danger"
					type="button"
					onclick={() => {
						disableConfirm = true;
						disableTyped = '';
						disableMsg = '';
					}}>Disable account</button
				>
			{:else}
				<!-- Typed-email confirmation (new in T1551). Mirrors the
				     destructive-action pattern used elsewhere — admins
				     can't disable an account without typing the email. -->
				<div class="confirm-block">
					<p class="confirm-text">
						Type <code>{user.email}</code> to confirm. The user will be signed out and
						unable to log back in until you re-enable.
					</p>
					<input
						type="text"
						class="confirm-input"
						placeholder={user.email}
						bind:value={disableTyped}
						aria-label="Type the user's email to confirm disable"
					/>
					<div class="row">
						<button
							class="btn btn-danger"
							type="button"
							disabled={disableSaving || disableTyped.trim() !== user.email}
							onclick={toggleDisable}
							title={disableTyped.trim() === user.email
								? 'Disable this account'
								: `Type ${user.email} to enable this button (paste tolerant)`}
							>{disableSaving ? 'Disabling…' : 'Disable'}</button
						>
						<button
							class="btn btn-sub"
							type="button"
							onclick={() => {
								disableConfirm = false;
								disableTyped = '';
							}}>Cancel</button
						>
					</div>
				</div>
			{/if}
		{:else}
			<button class="btn" type="button" disabled={disableSaving} onclick={toggleDisable}
				>{disableSaving ? 'Enabling…' : 'Re-enable account'}</button
			>
		{/if}
	</div>

	<!-- Plan + overrides -->
	<div class="field">
		<label class="field-label" for="settings-plan">Plan</label>
		<select id="settings-plan" bind:value={editPlan}>
			<option value="free">free</option>
			<option value="pro">pro</option>
			<option value="self-hosted">self-hosted</option>
		</select>
	</div>

	<div class="field">
		<div class="field-label">Plan overrides</div>
		<p class="hint">Empty = use plan default. Set a number to override the per-user limit for this user.</p>
		<div class="override-grid">
			{#each overrideFields as f (f.key)}
				<label class="override-field">
					<span class="override-label">{f.label}</span>
					<input
						type="text"
						inputmode="numeric"
						placeholder="default"
						bind:value={editOverrides[f.key]}
					/>
					<span class="override-hint">{f.hint}</span>
				</label>
			{/each}
		</div>

		<div class="field">
			<label class="field-label" for="settings-storage">Storage quota override</label>
			<input
				id="settings-storage"
				type="text"
				class="storage-input"
				placeholder="500 MB, 10 GB, or -1 for unlimited"
				bind:value={editStorageOverride}
				aria-describedby="storage-preview"
			/>
			<div id="storage-preview" class="hint" class:error={storageOverridePreview === 'Invalid input'}>
				{storageOverridePreview || 'Empty = use plan default'}
			</div>
			{#if storageOverrideError}<div class="msg error">{storageOverrideError}</div>{/if}
		</div>
	</div>

	<div class="save-row">
		<button class="btn btn-primary" type="button" disabled={saving} onclick={saveUser}
			>{saving ? 'Saving…' : 'Save plan + overrides'}</button
		>
		{#if saveMsg}<span class="msg" class:ok={saveMsg === 'Saved'}>{saveMsg}</span>{/if}
	</div>
</div>

<style>
	.settings-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.field-label {
		font-weight: 500;
		font-size: 0.9rem;
		color: var(--text-primary);
	}
	.hint {
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.hint.error,
	.msg.error {
		color: #ef4444;
	}
	.msg {
		font-size: 0.85rem;
		color: var(--text-muted);
	}
	.msg.ok {
		color: #10b981;
	}
	.row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		flex-wrap: wrap;
	}
	.role-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		flex-wrap: wrap;
	}
	.role-confirm {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		flex-wrap: wrap;
	}
	.role-confirm-msg {
		font-size: 0.85rem;
		color: var(--text-muted);
	}
	.btn {
		padding: 6px 12px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		cursor: pointer;
		font-size: 0.85rem;
	}
	.btn:hover {
		background: var(--bg-tertiary);
	}
	.btn:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}
	.btn[disabled] {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.btn-primary {
		background: var(--accent-blue);
		color: white;
		border-color: var(--accent-blue);
	}
	.btn-primary:hover {
		filter: brightness(1.1);
	}
	.btn-danger {
		color: #ef4444;
		border-color: color-mix(in srgb, #ef4444 50%, transparent);
	}
	.btn-danger:hover {
		background: color-mix(in srgb, #ef4444 10%, transparent);
	}
	.btn-sub {
		background: transparent;
		color: var(--text-muted);
		border: 1px dashed var(--border);
	}
	select,
	input[type='text'] {
		padding: 6px 10px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.9rem;
		outline: none;
	}
	select:focus,
	input[type='text']:focus {
		border-color: var(--accent-blue);
	}
	.override-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
		gap: var(--space-3);
	}
	.override-field {
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.override-label {
		font-size: 0.85rem;
		color: var(--text-primary);
	}
	.override-hint {
		font-size: 0.75rem;
		color: var(--text-muted);
	}
	.storage-input {
		max-width: 320px;
	}
	.temp-password {
		font-family: var(--font-mono, monospace);
		padding: var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.85rem;
	}
	.temp-password code {
		font-weight: 600;
	}
	.confirm-block {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		border: 1px solid color-mix(in srgb, #ef4444 30%, transparent);
		border-radius: var(--radius);
		background: color-mix(in srgb, #ef4444 5%, transparent);
	}
	.confirm-text {
		margin: 0;
		font-size: 0.85rem;
		color: var(--text-primary);
	}
	.confirm-input {
		max-width: 320px;
	}
	.save-row {
		display: flex;
		gap: var(--space-3);
		align-items: center;
	}
</style>
