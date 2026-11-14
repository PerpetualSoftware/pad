<!--
	GlobalBottomBar — the compact phone bar OUTSIDE a workspace (TASK-4315 PR 4):
	Workspaces · Chat · You. Inside a workspace BottomNav is the bar. This one
	serves the console, the workspace picker and the standalone /messages page, so
	Chat is one tap away from everywhere a phone can be while signed in. The root
	layout mounts it only for non-auth, non-share pages and outside every `inert`
	wrapper; it gates itself on phone, signed in, keyboard down.

	It shares ChatSlot with BottomNav and the same `nav.bottom-nav` class, so the
	docked sheets' Tab trap and the CommandPalette reflow find it. With Chat off
	there is no Chat slot (nothing to open, nothing to count).
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import { goto } from '$app/navigation';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import ChatSlot from '$lib/components/layout/ChatSlot.svelte';
	import YouSheet from '$lib/components/layout/YouSheet.svelte';

	let youOpen = $state(false);

	let shown = $derived(uiStore.isMobile && authStore.authenticated && !uiStore.keyboardVisible);

	// With the Chat sheet open, another slot waits for it to close, then runs.
	function afterChat(run: () => void) {
		if (!chatSurface.closeThen(run)) run();
	}
	function toggleYou() {
		afterChat(() => (youOpen = !youOpen));
	}
	function openWorkspaces(e: MouseEvent) {
		youOpen = false;
		if (chatSurface.closeThen(() => void goto('/'))) e.preventDefault();
	}

	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-bottom-nav', shown);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-bottom-nav');
	});
</script>

{#if uiStore.isMobile && authStore.authenticated}
	{#if !uiStore.keyboardVisible}
		<nav class="bottom-nav" aria-label="Primary">
			<a class="bn-item" href="/" onclick={openWorkspaces}>
				<span class="bn-icon" aria-hidden="true">🗂️</span>
				<span class="bn-label">Workspaces</span>
			</a>
			{#if chatSetting.enabled}
				<ChatSlot onopen={() => (youOpen = false)} />
			{/if}
			<button
				class="bn-item"
				class:active={youOpen}
				type="button"
				onclick={toggleYou}
				aria-haspopup="dialog"
				aria-expanded={youOpen}
			>
				<span class="bn-icon" aria-hidden="true">👤</span>
				<span class="bn-label">You</span>
			</button>
		</nav>
	{/if}
	<YouSheet open={youOpen} onclose={() => (youOpen = false)} />
{/if}

<style>
	.bottom-nav {
		position: fixed;
		bottom: 0;
		left: 0;
		right: 0;
		z-index: 40; /* sheets dock above at 45/46 but never cover this bar */
		display: flex;
		align-items: stretch;
		height: calc(var(--bottom-nav-height) + env(safe-area-inset-bottom, 0px));
		padding-bottom: env(safe-area-inset-bottom, 0px);
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
	}
	.bn-item {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 3px;
		background: none;
		border: none;
		cursor: pointer;
		padding: 0;
		color: var(--text-muted);
		text-decoration: none;
		font-family: var(--font-ui);
		min-width: 0;
	}
	.bn-item.active {
		color: var(--text-primary);
	}
	.bn-icon {
		font-size: 1.2em;
		line-height: 1;
	}
	.bn-label {
		font-size: 0.68em;
		font-weight: 500;
		max-width: 100%;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
