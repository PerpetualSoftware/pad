<script lang="ts">
	import { onMount, tick, untrack } from 'svelte';
	import { onNavigate } from '$app/navigation';
	import { page } from '$app/state';
	import ChatInboxView from '$lib/components/chat/ChatInboxView.svelte';
	import ChatThreadView from '$lib/components/chat/ChatThreadView.svelte';
	import ChatOff from '$lib/components/chat/ChatOff.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { chatSetting } from '$lib/stores/chatSetting.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';
	import { coveredPage } from '$lib/stores/coveredPage.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { topEscapePriority, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { createFocusReturn } from '$lib/a11y/focusReturn';
	import { isBlockedByModal } from '$lib/a11y/viewerBackdrop';
	import { nextTrapTargetAcross } from '$lib/collections/paneFocus';

	// The Chat surface (TASK-4315 PR 3b), mounted once from the root layout and
	// OUTSIDE every `inert` wrapper it causes. It renders only while a shallow
	// Chat navigation is open (chatSurface), so a closed host has no inbox, no
	// thread, no sentinel and no poller. The page underneath stays mounted.
	//
	// Wide enough: a 440px column beside the work (a labelled complementary
	// region; the body gets --chat-dock-width and shrinks). Otherwise a right
	// drawer, at most 480px. One element serves both, so a resize across the
	// threshold swaps the layout without remounting the one view inside.
	const DOCK_PX = 440;
	const MIN_CONTENT_PX = 560;

	let width = $state(typeof window === 'undefined' ? 1280 : window.innerWidth);
	let sidebarPx = $state(260);
	let panelEl = $state<HTMLElement | null>(null);

	function measure() {
		width = window.innerWidth;
		const v = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--sidebar-width'));
		sidebarPx = Number.isFinite(v) ? v : 260;
	}
	onMount(measure);

	// A page write held for the open Chat, and a heal queued behind its close, belong
	// to the page they were made on. A real navigation away (not Chat's own shallow
	// ones) abandons them (a heal would override the user's destination), and this runs
	// BEFORE Kit commits and clears page.shallow, so the close that follows cannot
	// flush a write for a page being left. A navigation the draft guard cancels
	// never gets here, so that write survives. Query-only navigations keep it.
	onNavigate((nav) => {
		if (nav.shallow) return;
		chatSurface.dropDeferredExcept(nav.to?.url.pathname ?? null);
	});

	const target = $derived(authStore.authenticated ? chatSurface.target : null);
	// The console has no sidebar to subtract.
	const sidebarShown = $derived(
		!uiStore.isMobile && uiStore.sidebarOpen && !page.url.pathname.startsWith('/console')
	);
	const docked = $derived(width - (sidebarShown ? sidebarPx : 0) >= DOCK_PX + MIN_CONTENT_PX);
	const drawer = $derived(target !== null && !docked);

	// Another overlay over the drawer (or over a dock) hides the thread: the
	// drawer's own cover does not count against it.
	const active = $derived(coveredPage.count <= (drawer ? 1 : 0));

	$effect(() => {
		if (!target || !docked) return;
		document.body.style.setProperty('--chat-dock-width', `${DOCK_PX}px`);
		return () => document.body.style.removeProperty('--chat-dock-width');
	});

	// The drawer takes the page out of the screen-reader tree, like a docked sheet.
	$effect(() => {
		if (!drawer) return;
		return coveredPage.enter({ keepBottomNav: false });
	});

	// Focus moves in on open and back to the invoker on close.
	const focusReturn = createFocusReturn();
	let wasOpen = false;
	let shownView = '';
	$effect(() => {
		const isOpen = target !== null;
		const view = target ? (target.kind === 'thread' ? `thread:${target.n}` : target.kind) : '';
		untrack(() => {
			if (isOpen && !wasOpen) {
				focusReturn.save();
				void tick().then(() => panelEl?.focus({ preventScroll: true }));
			} else if (!isOpen && wasOpen) {
				void tick().then(() => focusReturn.restore());
			} else if (isOpen && view !== shownView) {
				// Inbox to thread (or back) inside an open host destroys the link that was
				// clicked, and focus falls to <body>. Bring it into the new view then; focus
				// the reader put somewhere else is left alone.
				void tick().then(() => {
					const at = document.activeElement;
					if (panelEl && (!at || at === document.body || !at.isConnected)) {
						panelEl.focus({ preventScroll: true });
					}
				});
			}
			wasOpen = isOpen;
			shownView = view;
		});
	});
	$effect(() => () => void tick().then(() => focusReturn.restore()));

	function onKeydown(e: KeyboardEvent) {
		if (!drawer || !panelEl) return;
		if (e.key === 'Tab') {
			if (isBlockedByModal(panelEl, e)) return;
			const next = nextTrapTargetAcross([panelEl], document.activeElement, e.shiftKey);
			if (next) {
				e.preventDefault();
				next.focus({ preventScroll: true });
			}
			return;
		}
		// A held Escape repeats; only the first press acts, and only when nothing
		// frontmost (a confirm dialog, a menu's viewer) owns it.
		if (e.key !== 'Escape' || e.repeat || isBlockedByModal(panelEl, e)) return;
		// A layer inside the drawer (the message menu, portaled to <body>) owns the key
		// while it is registered on the Escape stack, and a control or an IME composition
		// that already used it has spoken for it. Page layers under the drawer (the list
		// marker, the pane) sit below the menu priority and do not count.
		if (e.defaultPrevented || e.isComposing) return;
		const front = topEscapePriority();
		if (front !== null && front >= ESCAPE_PRIORITY.menu) return;
		chatSurface.close();
	}
</script>

<svelte:window onresize={measure} onkeydown={onKeydown} />

<!-- The docked column sits beside the page; the global rule below makes room for it. -->
{#if target}
	{#if drawer}
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="chat-backdrop" onclick={() => chatSurface.close()}></div>
	{/if}
	<div
		bind:this={panelEl}
		class="chat-panel"
		class:docked
		class:drawer
		data-chat-host
		role={docked ? 'complementary' : 'dialog'}
		aria-modal={docked ? undefined : 'true'}
		aria-label="Chat"
		tabindex="-1"
	>
		<div class="chat-body">
			{#if !chatSetting.enabled}
				<button type="button" class="close-chat" onclick={() => chatSurface.close()}>Close Chat</button>
				<ChatOff />
			{:else if target.kind === 'inbox'}
				<ChatInboxView variant="host" />
			{:else}
				<ChatThreadView n={target.kind === 'thread' ? target.n : null} variant="host" {active} />
			{/if}
		</div>
	</div>
{/if}

<style>
	:global(body) {
		padding-right: var(--chat-dock-width, 0px);
	}
	.chat-panel {
		position: fixed;
		top: 0;
		right: 0;
		bottom: 0;
		z-index: 40;
		display: flex;
		flex-direction: column;
		box-sizing: border-box;
		background: var(--bg-primary);
		border-left: 1px solid var(--border);
		outline: none;
	}
	.chat-panel.docked {
		width: 440px;
	}
	.chat-panel.drawer {
		width: min(480px, 100vw);
		box-shadow: var(--shadow-md);
	}
	.chat-backdrop {
		position: fixed;
		inset: 0;
		z-index: 39;
		background: rgba(0, 0, 0, 0.4);
	}
	.chat-body {
		flex: 1;
		min-height: 0;
		display: flex;
		flex-direction: column;
		overflow-y: auto;
		padding: var(--space-4);
		padding-bottom: calc(var(--space-4) + env(safe-area-inset-bottom));
	}
</style>
