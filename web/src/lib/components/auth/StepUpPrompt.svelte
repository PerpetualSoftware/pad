<script lang="ts">
	import { onDestroy, onMount, tick } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import type { StepUpStatus, StepUpProof } from '$lib/types';

	let { factor, onverified }: { factor: StepUpStatus['factor']; onverified: () => void } = $props();
	const uid = $props.id();
	const headingId = `${uid}-heading`;
	const errorId = `${uid}-error`;
	let code = $state('');
	let password = $state('');
	let recovery = $state(false);
	let busy = $state(false);
	let sent = $state(false);
	let error = $state('');
	let headingEl = $state<HTMLHeadingElement>();
	let passwordEl = $state<HTMLInputElement>();
	let codeEl = $state<HTMLInputElement>();
	let sendEl = $state<HTMLButtonElement>();
	let mounted = true;
	onDestroy(() => { mounted = false; });

	// The prompt replaces the edit form, so focus would otherwise fall to <body>.
	onMount(() => {
		if (factor === 'password') passwordEl?.focus();
		else if (factor === 'totp') codeEl?.focus();
		else if (factor === 'email') sendEl?.focus();
		else headingEl?.focus();
	});

	async function focusCode() {
		await tick();
		if (mounted) codeEl?.focus();
	}
	function toggleRecovery() {
		recovery = !recovery; code = ''; error = '';
		void focusCode();
	}
	async function sendCode() {
		const sameIdentity = authStore.identityFence();
		const current = () => mounted && sameIdentity();
		let ok = false;
		busy = true; error = '';
		try {
			await api.auth.stepUp.sendEmailCode();
			if (current()) { sent = true; ok = true; }
		} catch (err) {
			if (current()) error = err instanceof Error ? err.message : 'Could not send the code.';
		} finally {
			if (current()) busy = false;
		}
		// The input is disabled while busy, so focus it only once busy has cleared.
		if (ok && current()) void focusCode();
	}
	async function verify(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		const sameIdentity = authStore.identityFence();
		const current = () => mounted && sameIdentity();
		const proof: StepUpProof = factor === 'password' ? { password } : recovery ? { recovery_code: code.trim() } : { code: code.trim() };
		busy = true; error = '';
		try {
			await api.auth.stepUp.verify(proof);
			if (!current()) return;
			password = ''; code = '';
			onverified();
		} catch (err) {
			if (current()) error = err instanceof Error ? err.message : 'Verification failed.';
		} finally {
			if (current()) busy = false;
		}
	}
</script>

<div class="step-up">
	<h3 id={headingId} bind:this={headingEl} tabindex="-1">Verify it’s you</h3>
	{#if factor === 'unavailable'}
		<p>Turn on two-factor authentication to change key access.</p>
	{:else}
		<form onsubmit={verify} aria-labelledby={headingId}>
			<p>Verify your identity to increase this key’s access. Verification lasts 10 minutes in this session.</p>
			{#if factor === 'password'}
				<label for="step-up-password">Password</label>
				<input id="step-up-password" type="password" autocomplete="current-password" bind:this={passwordEl} bind:value={password} disabled={busy} required aria-invalid={error ? 'true' : undefined} aria-describedby={error ? errorId : undefined} />
			{:else}
				{#if factor === 'email'}
					<p>A single-use code will be emailed to you. It expires in 10 minutes.</p>
					<div class="send-row">
						<Button variant="secondary" bind:element={sendEl} onclick={sendCode} disabled={busy}>{sent ? 'Send another code' : 'Send code'}</Button>
						{#if sent}<p role="status" class="status">Code sent. Check your email.</p>{/if}
					</div>
				{/if}
				<label for="step-up-code">{recovery ? 'Recovery code' : factor === 'totp' ? 'Authenticator code' : 'Email code'}</label>
				<input id="step-up-code" type="text" inputmode={recovery ? 'text' : 'numeric'} autocomplete="one-time-code" bind:this={codeEl} bind:value={code} disabled={busy} required aria-invalid={error ? 'true' : undefined} aria-describedby={error ? errorId : undefined} />
				{#if factor === 'totp'}
					<div class="toggle-row">
						<Button variant="ghost" onclick={toggleRecovery} disabled={busy}>{recovery ? 'Use an authenticator code' : 'Use a recovery code'}</Button>
					</div>
				{/if}
			{/if}
			{#if error}<p id={errorId} role="alert" class="error">{error}</p>{/if}
			<div class="actions"><Button type="submit" disabled={busy || !(factor === 'password' ? password : code.trim())}>{busy ? 'Verifying…' : 'Verify'}</Button></div>
		</form>
	{/if}
</div>

<style>
	h3 { margin: 0 0 .25rem; font-size: 1rem; font-weight: 600; }
	h3:focus { outline: none; }
	h3:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; }
	label { display: block; margin: 1rem 0 .4rem; font-weight: 500; }
	input { width: 100%; box-sizing: border-box; padding: .6rem; border: 1px solid var(--border); border-radius: 6px; background: var(--bg-primary); color: var(--text-primary); }
	p { line-height: 1.5; }
	.send-row { display: flex; flex-direction: column; align-items: flex-start; gap: .5rem; }
	.send-row .status { margin: 0; }
	.toggle-row { margin-top: .5rem; }
	.error { margin: .5rem 0 0; color: var(--accent-red); }
	.actions { display: flex; justify-content: flex-end; margin-top: 1rem; }
</style>
