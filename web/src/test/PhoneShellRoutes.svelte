<script lang="ts">
	import { page } from '$app/state';
	import RootLayout from '../routes/+layout.svelte';
	import ConsoleLayout from '../routes/(console)/+layout.svelte';
	import Home from '../routes/(console)/+page.svelte';
	import Needs from '../routes/(console)/needs/+page.svelte';
	import SettingsLayout from '../routes/(console)/settings/+layout.svelte';
	import Account from '../routes/(console)/settings/account/+page.svelte';
	import MessagesLayout from '../routes/messages/+layout.svelte';
	import Messages from '../routes/messages/+page.svelte';
</script>

<RootLayout>
	{#if page.url.pathname === '/messages'}
		<MessagesLayout><Messages /></MessagesLayout>
	{:else}
		<ConsoleLayout>
			{#if page.url.pathname === '/'}
				<Home />
			{:else if page.url.pathname === '/needs'}
				<Needs />
			{:else if page.url.pathname === '/settings/account'}
				<SettingsLayout><Account /></SettingsLayout>
			{/if}
		</ConsoleLayout>
	{/if}
</RootLayout>
