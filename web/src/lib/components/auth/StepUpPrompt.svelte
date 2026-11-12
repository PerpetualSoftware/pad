<script lang="ts">
 import { onDestroy } from 'svelte';
 import { api } from '$lib/api/client';
 import { authStore } from '$lib/stores/auth.svelte';
 import Button from '$lib/components/common/Button.svelte';
 import type { StepUpStatus, StepUpProof } from '$lib/types';

 let { factor, onverified }: { factor: StepUpStatus['factor']; onverified: () => void } = $props();
 let code = $state('');
 let password = $state('');
 let recovery = $state(false);
 let busy = $state(false);
 let sent = $state(false);
 let error = $state('');
 let mounted = true;
 onDestroy(() => { mounted = false; });

 async function sendCode() {
  const sameIdentity = authStore.identityFence();
  const current = () => mounted && sameIdentity();
  busy = true; error = '';
  try {
   await api.auth.stepUp.sendEmailCode();
   if (current()) sent = true;
  } catch (err) {
   if (current()) error = err instanceof Error ? err.message : 'Could not send the code.';
  } finally {
   if (current()) busy = false;
  }
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

{#if factor === 'unavailable'}
 <p>Turn on two-factor authentication to change key access.</p>
{:else}
 <form onsubmit={verify}>
  <p>Verify your identity to increase this key’s access. Verification lasts 10 minutes in this session.</p>
  {#if factor === 'password'}
   <label for="step-up-password">Password</label>
   <input id="step-up-password" type="password" autocomplete="current-password" bind:value={password} disabled={busy} required />
  {:else}
   {#if factor === 'email'}
    <p>A single-use code will be emailed to you. It expires in 10 minutes.</p>
    <Button variant="secondary" onclick={sendCode} disabled={busy}>{sent ? 'Send another code' : 'Send code'}</Button>
    {#if sent}<p role="status">Code sent. Check your email.</p>{/if}
   {/if}
   <label for="step-up-code">{recovery ? 'Recovery code' : factor === 'totp' ? 'Authenticator code' : 'Email code'}</label>
   <input id="step-up-code" type="text" inputmode={recovery ? 'text' : 'numeric'} autocomplete="one-time-code" bind:value={code} disabled={busy} required />
   {#if factor === 'totp'}
    <Button variant="ghost" onclick={() => { recovery = !recovery; code = ''; error = ''; }} disabled={busy}>{recovery ? 'Use an authenticator code' : 'Use a recovery code'}</Button>
   {/if}
  {/if}
  {#if error}<p role="alert" class="error">{error}</p>{/if}
  <div class="actions"><Button type="submit" disabled={busy || !(factor === 'password' ? password : code.trim())}>{busy ? 'Verifying…' : 'Verify'}</Button></div>
 </form>
{/if}

<style>
 label { display: block; margin: 1rem 0 .4rem; font-weight: 500; }
 input { width: 100%; box-sizing: border-box; padding: .6rem; border: 1px solid var(--border); border-radius: 6px; background: var(--bg-primary); color: var(--text-primary); }
 p { line-height: 1.5; }
 .error { color: var(--accent-red); }
 .actions { display: flex; justify-content: flex-end; margin-top: 1rem; }
</style>
