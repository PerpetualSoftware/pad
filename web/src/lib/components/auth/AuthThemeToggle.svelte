<script lang="ts">
	// Light/dark toggle for the auth pages (TASK-3509). It writes the same
	// preference the app reads: localStorage 'pad-theme' and data-theme on
	// <html>, which the root layout restores on every load. So a choice made on
	// /login is the theme the app opens in after signing in.
	import { onMount } from 'svelte';

	let theme = $state<'dark' | 'light'>('dark');

	onMount(() => {
		// The root layout has already applied the saved choice, or light for a
		// light system preference; no attribute means the dark default.
		const attr = document.documentElement.getAttribute('data-theme');
		theme = attr === 'light' || (!attr && window.matchMedia('(prefers-color-scheme: light)').matches) ? 'light' : 'dark';
	});

	function toggle() {
		theme = theme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', theme);
		try {
			localStorage.setItem('pad-theme', theme);
		} catch {
			// Storage unavailable: the page still switches, the choice is not kept.
		}
	}
</script>

<button
	type="button"
	class="auth-theme-toggle"
	onclick={toggle}
	aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
	title={theme === 'dark' ? 'Light theme' : 'Dark theme'}
	data-testid="auth-theme-toggle"
>
	{#if theme === 'dark'}
		<!-- sun: what a click switches to -->
		<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
			<circle cx="12" cy="12" r="4" />
			<path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
		</svg>
	{:else}
		<!-- moon -->
		<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
			<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
		</svg>
	{/if}
</button>

<style>
	.auth-theme-toggle {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 36px;
		height: 36px;
		border-radius: 999px;
		border: 1px solid var(--border);
		background: transparent;
		color: var(--text-secondary);
		cursor: pointer;
		flex-shrink: 0;
		transition: color 150ms ease, border-color 150ms ease;
	}
	.auth-theme-toggle:hover {
		color: var(--text-primary);
		border-color: var(--border-strong);
	}
	.auth-theme-toggle:focus-visible {
		outline: 2px solid var(--accent-primary);
		outline-offset: 2px;
	}
</style>
