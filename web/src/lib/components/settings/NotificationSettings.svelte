<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import type { NotificationSettings } from '$lib/types';

	// Which events send the caller a push notification (TASK-3940). Every save
	// is a full replace of all seven values, so the body is built from KEYS
	// rather than spread from the response, and writes are serialized: two
	// overlapping full-replace PUTs would lose one of the edits.
	const uid = $props.id();
	const id = `notification-settings-${uid}`;

	const ROWS: { key: keyof NotificationSettings; label: string; hint?: string }[] = [
		{ key: 'push_enabled', label: 'Push notifications' },
		{ key: 'notify_assigned', label: 'Assigned to you' },
		{ key: 'notify_comments', label: 'Comments on your items' },
		{ key: 'notify_replies', label: 'Replies to your comments' },
		{ key: 'notify_reminders', label: 'Reminders' },
		{ key: 'notify_mentions', label: 'Mentions' },
		{
			key: 'include_agent_activity',
			label: 'Include activity by agents',
			hint: "Off by default, so agents working in your workspace don't buzz your phone."
		}
	];

	let settings = $state<NotificationSettings | null>(null);
	let loading = $state(true);
	let loadError = $state('');
	let saveError = $state('');
	let saving = $state(false);

	async function load() {
		loading = true;
		loadError = '';
		try {
			settings = await api.notificationSettings.get();
		} catch (e) {
			loadError = e instanceof Error ? e.message : 'Could not load notification settings.';
		} finally {
			loading = false;
		}
	}

	async function toggle(key: keyof NotificationSettings, input: HTMLInputElement) {
		if (!settings) return;
		// aria-disabled does not block activation, so the guard lives here. The
		// box has already flipped in the DOM and `checked` is one-way, so put it
		// back to the stored value.
		if (saving) {
			input.checked = settings[key];
			return;
		}
		const checked = input.checked;
		const before = settings;
		const next = {} as NotificationSettings;
		for (const row of ROWS) next[row.key] = row.key === key ? checked : before[row.key];
		settings = next;
		saving = true;
		saveError = '';
		try {
			const saved = await api.notificationSettings.put(next);
			const confirmed = {} as NotificationSettings;
			for (const row of ROWS) confirmed[row.key] = saved[row.key];
			settings = confirmed;
		} catch (e) {
			settings = before;
			saveError = e instanceof Error ? e.message : 'Could not save notification settings.';
		} finally {
			saving = false;
		}
	}

	onMount(() => {
		// The page renders this section after its own load, so the browser has
		// already resolved the hash by the time the target exists. Scroll once.
		if (location.hash === '#notifications') {
			document.getElementById('notifications')?.scrollIntoView();
		}
		load();
	});
</script>

<p class="lead">
	Choose what sends a push notification to your phone. Notifications never include item titles or
	comment text.
</p>

{#if loading}
	<p class="status" aria-busy="true">Loading notification settings...</p>
{:else if loadError || !settings}
	<p class="error" role="alert">{loadError || 'Could not load notification settings.'}</p>
	<button type="button" class="retry" onclick={load}>Retry</button>
{:else}
	<div class="rows" class:off={!settings.push_enabled}>
		{#each ROWS as row, i (row.key)}
			<div class="toggle" class:master={i === 0}>
				<input
					id="{id}-{row.key}"
					type="checkbox"
					checked={settings[row.key]}
					aria-disabled={saving}
					aria-describedby={row.hint ? `${id}-${row.key}-hint` : undefined}
					onchange={(e) => toggle(row.key, e.currentTarget)}
				/>
				<div>
					<label for="{id}-{row.key}">{row.label}</label>
					{#if row.hint}
						<p id="{id}-{row.key}-hint" class="desc">{row.hint}</p>
					{/if}
				</div>
			</div>
			{#if i === 0 && !settings.push_enabled}
				<p class="desc note">Push is off, so nothing is sent to your devices.</p>
			{/if}
		{/each}
	</div>
	{#if saveError}
		<p class="error" role="alert">{saveError}</p>
	{/if}
{/if}

<style>
	.lead,
	.status {
		margin-bottom: var(--space-3);
		color: var(--text-secondary);
	}

	.rows {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.rows.off .toggle:not(.master) {
		opacity: 0.6;
	}

	.toggle {
		display: flex;
		align-items: flex-start;
		gap: var(--space-2);
	}

	input {
		margin-top: 3px;
	}

	label {
		font-weight: 500;
		color: var(--text-primary);
		cursor: pointer;
	}

	.desc {
		margin-top: var(--space-1);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.note {
		margin-top: 0;
	}

	.error {
		margin-top: var(--space-2);
		color: var(--accent-red);
	}

	.retry {
		margin-top: var(--space-2);
	}
</style>
