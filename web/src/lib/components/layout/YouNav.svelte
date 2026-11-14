<script lang="ts">
	import { untrack } from 'svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { chatStore } from '$lib/stores/chat.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import UnreadBadge from '$lib/components/chat/UnreadBadge.svelte';
	const todayCount = $derived(homeOverview.agenda?.entries.filter(e => e.day === homeOverview.today && (e.type === 'schedule' || e.type === 'date')).length);
	$effect(() => { const path = page.url.pathname; if (authStore.authenticated) untrack(() => { void homeOverview.load(path); }); });
</script>
<nav aria-label="You" class="you-nav">
	<div class="you-heading"><h2>You</h2><button aria-label="Quick capture" title="Quick capture (C)" onclick={() => uiStore.requestQuickAdd()}>+</button></div>
	{#each [{ href: '/', icon: '🏠', label: 'Home' }, { href: '/needs', icon: '📥', label: 'Needs you' }, { href: '/agenda', icon: '📅', label: 'Agenda' }] as link}
		<a href={link.href} class:active={page.url.pathname === link.href} aria-current={page.url.pathname === link.href ? 'page' : undefined}>
			<span aria-hidden="true">{link.icon}</span><span class="label">{link.label}</span>
			{#if link.href === '/needs' && homeOverview.needs}<span class="count needs">{homeOverview.needs.total}</span>{/if}
			{#if link.href === '/agenda' && todayCount !== undefined}<span class="count">{todayCount} today</span>{/if}
		</a>
	{/each}
	{#if authStore.authenticated && chatSetting.enabled}
		<a href="/messages" class:active={page.url.pathname.startsWith('/messages') || chatSurface.isOpen} aria-current={page.url.pathname.startsWith('/messages') ? 'page' : undefined} aria-label={chatStore.unreadTotal > 0 ? `Chat, ${chatStore.unreadTotal} unread` : 'Chat'} onclick={e => chatSurface.handleLinkClick(e, '/messages')}>
			<span aria-hidden="true">💬</span><span class="label">Chat</span><UnreadBadge count={chatStore.unreadTotal} />
		</a>
	{/if}
</nav>
<style>
	.you-nav { flex-shrink: 0; padding-bottom: var(--space-4); border-bottom: 1px solid var(--border); }
	.you-heading { display: flex; align-items: center; justify-content: space-between; }
	.you-heading button { color: var(--text-muted); padding: 0 var(--space-3); background: none; border: 0; cursor: pointer; }
	h2 { margin: var(--space-1) var(--space-3) var(--space-2); font-size: 0.65rem; text-transform: uppercase; color: var(--text-muted); letter-spacing: .04em; }
	a { display: flex; align-items: center; gap: var(--space-2); padding: var(--space-2) var(--space-3); margin-bottom: 2px; border-radius: var(--radius); color: var(--text-secondary); font-size: .8rem; text-decoration: none; min-height: 34px; }
	a:hover { background: var(--bg-hover); } a.active { background: color-mix(in srgb, var(--accent-blue) 20%, transparent); color: var(--accent-blue); }
	.label { flex: 1; } .count { font-size: .65rem; border-radius: 99px; background: var(--bg-tertiary); padding: 1px 6px; color: var(--text-muted); }
	.needs { background: color-mix(in srgb, var(--accent-amber) 14%, transparent); color: var(--accent-amber); }
</style>
