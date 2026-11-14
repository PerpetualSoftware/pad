<script lang="ts">
	import { onDestroy } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';
	import { portal } from '$lib/utils/portalAction';
	import { phoneNav } from './phoneNav.svelte';
	import AccountSheet from './AccountSheet.svelte';
	const name = $derived(authStore.user?.name || authStore.user?.email || '');
	function toggle() {
		const run = () => {
			uiStore.closeSearch();
			phoneNav.sheet = phoneNav.sheet === 'account' ? null : 'account';
		};
		if (!chatSurface.closeThen(run)) run();
	}
	onDestroy(() => { if (phoneNav.sheet === 'account') phoneNav.sheet = null; });
</script>

{#if uiStore.isMobile && authStore.authenticated}
	<button class="account-avatar" style:background={avatarColor(name)} aria-label="Account" aria-haspopup="dialog" aria-expanded={phoneNav.sheet === 'account'} onclick={toggle}>
		{avatarInitial(name)}
	</button>
	<!-- The trigger's header becomes inert under the sheet; the sheet must stay live. -->
	<div use:portal>
		<AccountSheet open={phoneNav.sheet === 'account'} onclose={() => phoneNav.sheet = null} />
	</div>
{/if}
<style>
	.account-avatar { display: flex; align-items: center; justify-content: center; flex-shrink: 0; width: 32px; height: 32px; border: 0; border-radius: 50%; color: var(--text-on-accent); font-weight: 700; cursor: pointer; }
</style>
