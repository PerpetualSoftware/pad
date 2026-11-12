<script lang="ts">
	import { onDestroy } from 'svelte';
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { describeTokenScopes } from '$lib/settings/tokenScopes';
	import type { APIToken, APITokenUpdate, StepUpStatus } from '$lib/types';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import StepUpPrompt from './StepUpPrompt.svelte';

	let { token, onclose, onupdated }: { token: APIToken | null; onclose: () => void; onupdated: (token: APIToken) => void } = $props();
	let name = $state('');
	let access = $state<'read' | 'write' | null>(null);
	let busy = $state(false);
	let error = $state('');
	let stepUp = $state<StepUpStatus | null>(null);
	let pending = $state<APITokenUpdate | null>(null);
	// Plain counter invalidates settlements when a dialog is closed or reopened.
	let generation = 0;
	onDestroy(() => { generation++; });
	$effect(() => {
		const target = token;
		generation++;
		name = target?.name ?? '';
		const level = describeTokenScopes(target?.scopes).level;
		access = level === 'none' ? null : level;
		busy = false; error = ''; stepUp = null; pending = null;
	});
	function close() { generation++; onclose(); }
	async function update(input: APITokenUpdate, allowPrompt: boolean) {
		const target = token;
		if (!target) return;
		const started = generation;
		const sameIdentity = authStore.identityFence();
		const current = () => sameIdentity() && started === generation && token === target;
		busy = true; error = '';
		try {
			const updated = await api.auth.tokens.update(target.id, input);
			// The server has applied the change, so the list takes it even when
			// the dialog was closed or reopened while the request ran; only the
			// dialog's own state is fenced by generation.
			if (!sameIdentity()) return;
			onupdated(updated);
			if (current()) close();
		} catch (err) {
			if (!current()) return;
			if (allowPrompt && err instanceof PadApiError && err.code === 'step_up_required') {
				try {
					const status = await api.auth.stepUp.status();
					if (!current()) return;
					pending = input; stepUp = status;
				} catch (statusError) {
					if (current()) error = statusError instanceof Error ? statusError.message : 'Could not load verification.';
				}
			} else {
				stepUp = null;
				error = err instanceof Error ? err.message : 'Could not update this key.';
			}
		} finally {
			if (current()) busy = false;
		}
	}
	function save(event: SubmitEvent) {
		event.preventDefault();
		if (!token || busy) return;
		const input: APITokenUpdate = { name: name.trim() };
		if (access && access !== describeTokenScopes(token.scopes).level) input.scopes = access === 'read' ? '["read"]' : '["*"]';
		void update(input, true);
	}
	function verified() {
		if (!pending) return;
		const input = pending;
		stepUp = null; pending = null;
		void update(input, false);
	}
</script>

<Modal open={!!token} onclose={close} ariaLabel="Edit API key access">
	<div class="content">
		<h2>Edit API key access</h2>
		{#if stepUp}
			<StepUpPrompt factor={stepUp.factor} onverified={verified} />
		{:else}
			<form onsubmit={save}>
				<label for="edit-token-name">Key name</label>
				<input id="edit-token-name" type="text" bind:value={name} disabled={busy} required />
				<fieldset disabled={busy}>
					<legend>Access</legend>
					<label><input type="radio" name="edit-token-access" value="write" bind:group={access} />Read and write</label>
					<label><input type="radio" name="edit-token-access" value="read" bind:group={access} />Read only</label>
				</fieldset>
				{#if error}<p role="alert" class="error">{error}</p>{/if}
				<div class="actions"><Button type="submit" disabled={busy || !name.trim()}>{busy ? 'Saving…' : 'Save changes'}</Button></div>
			</form>
		{/if}
		<div class="actions"><Button variant="secondary" onclick={close}>Cancel</Button></div>
	</div>
</Modal>

<style>
	.content { padding: 1.5rem; }
	h2 { margin: 0 0 1rem; font-size: 1.2rem; }
	label { display: block; margin: .6rem 0; }
	input[type='text'] { width: 100%; box-sizing: border-box; padding: .6rem; border: 1px solid var(--border); border-radius: 6px; background: var(--bg-primary); color: var(--text-primary); }
	input[type='radio'] { margin-right: .5rem; }
	fieldset { margin: 1rem 0; border: 0; padding: 0; }
	legend { font-weight: 500; }
	.error { color: var(--accent-red); }
	.actions { display: flex; justify-content: flex-end; margin-top: 1rem; }
</style>
