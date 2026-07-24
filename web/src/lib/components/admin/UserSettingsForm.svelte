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
	import { untrack } from 'svelte';
	import { adminFetch, adminPatch, adminPost, type AdminUser } from '$lib/stores/admin.svelte';
	import { api } from '$lib/api/client';
	import Button from '$lib/components/common/Button.svelte';

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

	// Email force-verify override (PLAN-1933 DR-7 / TASK-1939). Admin-only,
	// web-console only. Surfaced only when the target is unverified.
	let verifySaving = $state(false);
	let verifyMsg = $state('');

	// Hydrate form state when the modal switches to a different user.
	//
	// Reactive-loop note: an earlier version of this effect read user.plan /
	// user.role / user.plan_overrides directly and wrote to editOverrides
	// (= {} then per-key mutation). Combined with the {#each overrideFields}
	// template using bind:value={editOverrides[f.key]}, the bind machinery
	// would re-subscribe every time editOverrides got a fresh proxy from
	// the `= {}` write, which scheduled the effect's tracking owner — that
	// looped on modal open and froze the UI.
	//
	// Fix: gate the hydration on user.id change ONLY (a primitive read, no
	// proxy churn). Then wrap all writes in untrack() so they don't add
	// themselves to this effect's dependency set. Reading user.* inside
	// untrack also makes the reset insensitive to incidental prop spreads
	// from the parent — onModalUserUpdated creates a new modalUser object
	// after every save, but we don't want to nuke unsaved form state on
	// that path. Form state is the authoritative source while the modal
	// is open; we only re-hydrate on a user *swap*.
	let lastHydratedUserId = $state<string | null>(null);
	$effect(() => {
		const uid = user.id;
		if (uid === lastHydratedUserId) return;
		untrack(() => {
			editPlan = user.plan || 'free';
			editRole = user.role || 'member';
			const ov = parsePlanOverrides(user.plan_overrides);
			const next: Record<string, string> = {};
			const extra: Record<string, number> = {};
			for (const f of overrideFields) {
				next[f.key] = f.key in ov ? String(ov[f.key]) : '';
			}
			for (const [k, v] of Object.entries(ov)) {
				if (!overrideFieldKeys.has(k)) extra[k] = v;
			}
			// Single assignment per state var — building the new objects
			// first and assigning once avoids the "reassign then mutate"
			// pattern that thrashed bind:value subscriptions.
			editOverrides = next;
			extraOverrides = extra;
			editStorageOverride = 'storage_bytes' in ov ? formatStorageBytes(ov.storage_bytes) : '';
			storageOverrideError = '';
			saveMsg = '';
			roleConfirm = false;
			roleMsg = '';
			resetConfirm = false;
			resetResult = null;
			resetError = '';
			disableConfirm = false;
			disableTyped = '';
			disableMsg = '';
			lastHydratedUserId = uid;
		});
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

	// Force-verify the user's email (admin override). Refetches the user so
	// the panel reflects the now-verified state (which hides this control).
	async function verifyEmail() {
		const userId = user.id;
		verifySaving = true;
		verifyMsg = '';
		try {
			await api.admin.verifyEmail(userId);
			const updated = await adminFetch(`/admin/users/${userId}`);
			onUserUpdated?.(updated as AdminUser);
			verifyMsg = 'Email marked verified';
		} catch (e) {
			verifyMsg = e instanceof Error ? e.message : 'Action failed';
		} finally {
			verifySaving = false;
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
					<Button
						variant="secondary"
						onclick={() => {
							roleConfirm = true;
							roleMsg = '';
						}}>Change Role</Button
					>
				{:else}
					<div class="role-confirm">
						<span class="role-confirm-msg">
							{roleAction()} <strong>{user.name || user.username}</strong> to {editRole}?
						</span>
						<Button
							variant="primary"
							disabled={roleSaving}
							onclick={changeRole}>{roleSaving ? 'Saving…' : 'Confirm'}</Button
						>
						<Button
							variant="ghost"
							onclick={() => {
								roleConfirm = false;
								editRole = user.role || 'member';
							}}>Cancel</Button
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
			<Button variant="secondary" onclick={() => (resetConfirm = true)}>Reset password</Button>
		{:else}
			<div class="row">
				<span>Send a password-reset email or generate a temporary password?</span>
				<Button
					variant="primary"
					disabled={resetSaving}
					onclick={resetPassword}>{resetSaving ? 'Working…' : 'Confirm'}</Button
				>
				<Button variant="ghost" onclick={() => (resetConfirm = false)}>Cancel</Button>
			</div>
		{/if}
	</div>

	<!-- Disable / enable -->
	<div class="field">
		<div class="field-label">Account status</div>
		{#if disableMsg}<div class="msg" class:error={disableMsg.includes('failed') || disableMsg.includes('Type the')}>{disableMsg}</div>{/if}
		{#if !user.disabled_at}
			{#if !disableConfirm}
				<Button
					variant="danger"
					onclick={() => {
						disableConfirm = true;
						disableTyped = '';
						disableMsg = '';
					}}>Disable account</Button
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
						<Button
							variant="danger"
							disabled={disableSaving || disableTyped.trim() !== user.email}
							onclick={toggleDisable}
							title={disableTyped.trim() === user.email
								? 'Disable this account'
								: `Type ${user.email} to enable this button (paste tolerant)`}
							>{disableSaving ? 'Disabling…' : 'Disable'}</Button
						>
						<Button
							variant="ghost"
							onclick={() => {
								disableConfirm = false;
								disableTyped = '';
							}}>Cancel</Button
						>
					</div>
				</div>
			{/if}
		{:else}
			<Button variant="secondary" disabled={disableSaving} onclick={toggleDisable}
				>{disableSaving ? 'Enabling…' : 'Re-enable account'}</Button
			>
		{/if}
	</div>

	<!-- Email verification — admin force-verify override (PLAN-1933 DR-7).
	     Shown only when the target is unverified; verifying hides the field. -->
	{#if !user.email_verified_at}
		<div class="field">
			<div class="field-label">Email verification</div>
			{#if verifyMsg}<div class="msg" class:error={verifyMsg === 'Action failed'}>{verifyMsg}</div>{/if}
			<p class="hint">
				This user hasn't confirmed their email. Marking it verified unblocks content
				mutations and invites for their account.
			</p>
			<Button variant="secondary" disabled={verifySaving} onclick={verifyEmail}
				>{verifySaving ? 'Verifying…' : 'Mark email verified'}</Button
			>
		</div>
	{/if}

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
		<Button variant="primary" disabled={saving} onclick={saveUser}
			>{saving ? 'Saving…' : 'Save plan + overrides'}</Button
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
		color: var(--accent-red);
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
		border: 1px solid color-mix(in srgb, var(--accent-red) 30%, transparent);
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--accent-red) 5%, transparent);
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
