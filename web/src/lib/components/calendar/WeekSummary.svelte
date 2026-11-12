<script lang="ts">
	import type { AgendaResponse } from '$lib/types';

	interface Props {
		/** The range on screen, which names the card. */
		range: 'week' | 'day' | 'month';
		data: AgendaResponse;
		/** Today in the viewer's zone, as `YYYY-MM-DD`. A day after it has not happened yet. */
		today: string;
	}

	let { range, data, today }: Props = $props();

	const id = $props.id();
	const WIDTH = 298;
	const HEIGHT = 44;
	/** What a day with nothing completed still draws, so an elapsed day never reads as a missing one. */
	const STUB = 3;
	const title = $derived(range === 'week' ? 'This week' : range === 'day' ? 'This day' : 'This month');

	// One occurrence can be both a still-owed row and a lapsed entry in the window; it is past its deadline once.
	const past = $derived.by(() => {
		const seen = new Set<string>();
		const add = (row: { workspace?: string; owner_username?: string; occurrence_key?: string; item_ref: string }) =>
			seen.add(`${row.workspace ?? ''}|${row.owner_username ?? ''}|${row.occurrence_key ?? row.item_ref}`);
		for (const row of data.priority_list) if (row.reason === 'deadline_still_owed') add(row);
		for (const entry of data.entries) if (entry.type === 'schedule' && entry.state === 'lapsed') add(entry);
		return seen.size;
	});
	const outstanding = $derived(`${data.priority_list.length}${data.priority_list_complete ? '' : '+'}`);

	const counts = $derived(data.counts_truncated ? [] : data.counts ?? []);
	const elapsed = $derived(counts.filter(item => item.day <= today));
	const completed = $derived(elapsed.reduce((sum, item) => sum + item.completed, 0));
	const top = $derived(Math.max(1, ...elapsed.map(item => item.completed)));
	const gap = $derived(counts.length > 10 ? 2 : 10);
	const slot = $derived(counts.length ? (WIDTH - gap * (counts.length - 1)) / counts.length : 0);
	const bars = $derived(counts.map((item, index) => {
		const height = item.completed > 0 ? Math.max(STUB, (item.completed / top) * (HEIGHT - 6)) : STUB;
		return { day: item.day, x: index * (slot + gap), height, empty: item.completed === 0, future: item.day > today };
	}));
	const last = $derived(elapsed.at(-1));

	// The API day is already local. UTC here preserves its calendar date for display.
	const at = (day: string) => new Date(`${day}T12:00:00Z`);
	const initial = new Intl.DateTimeFormat('en-GB', { weekday: 'narrow', timeZone: 'UTC' });
	const edge = new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', timeZone: 'UTC' });
	const rowDay = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
	const caption = $derived(range === 'week' ? counts.map(item => initial.format(at(item.day)))
		: range === 'month' && counts.length > 1 ? [edge.format(at(counts[0].day)), edge.format(at(counts.at(-1)!.day))] : []);
</script>

<section class="summary" aria-labelledby={`${id}-title`}>
	<h2 id={`${id}-title`}>{title}</h2>
	<div class="nums">
		<div class="stat" class:warn={past > 0} data-stat="past"><b>{past}</b><span>past deadline</span></div>
		<div class="stat" data-stat="outstanding"><b>{outstanding}</b><span>outstanding</span></div>
		{#if counts.length}<div class="stat" data-stat="completed"><b>{completed}</b><span>completed</span></div>{/if}
	</div>
	{#if data.counts_truncated}
		<p class="note">Counts unavailable for this window: too much activity to count.</p>
	{:else if counts.length}
		<div class="spark">
			<div class="legend">
				<span><i aria-hidden="true"></i>Completed per day</span>
				{#if last}<span>{last.open} open · burn-down {counts[0].burndown} → {last.burndown}</span>{/if}
			</div>
			<svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} width="100%" height={HEIGHT} preserveAspectRatio="none" aria-hidden="true">
				{#each bars as bar, index (index)}
					{#if !bar.future}
						<rect data-day={bar.day} data-empty={bar.empty} x={bar.x} y={HEIGHT - bar.height} width={slot} height={bar.height} rx={bar.empty ? 1.5 : Math.min(3, slot / 2)} />
					{/if}
				{/each}
			</svg>
			{#if caption.length}<div class="cap" class:each={range === 'week'} aria-hidden="true">{#each caption as text, index (index)}<span>{text}</span>{/each}</div>{/if}
		</div>
		<table class="sr-only">
			<caption>Daily counts</caption>
			<thead><tr><th scope="col">Day</th><th scope="col">Completed</th><th scope="col">Open at day end</th><th scope="col">Burn-down</th></tr></thead>
			<tbody>
				{#each counts as item, index (index)}
					<tr><th scope="row"><time datetime={item.day}>{rowDay.format(at(item.day))}</time></th><td>{item.completed}</td><td>{item.open}</td><td>{item.burndown}</td></tr>
				{/each}
			</tbody>
		</table>
	{/if}
</section>

<style>
	.summary { padding: var(--space-4); min-width: 0; }
	h2 { margin: 0 0 10px; font-size: 11.5px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; color: var(--text-secondary); }
	.nums { display: grid; grid-template-columns: repeat(3, 1fr); gap: var(--space-3); }
	.stat b { display: block; font-size: 22px; font-weight: 700; line-height: 1.15; font-variant-numeric: tabular-nums; color: var(--text-primary); }
	.stat span { font-size: 12px; color: var(--text-muted); }
	.stat.warn b { color: var(--accent-red); }
	.spark { margin-top: var(--space-3); }
	.legend { display: flex; flex-wrap: wrap; gap: 2px var(--space-3); margin-bottom: 6px; font-size: 11px; color: var(--text-muted); }
	.legend i { display: inline-block; width: 8px; height: 8px; margin-right: 4px; border-radius: 2px; background: var(--accent-primary); }
	svg { display: block; }
	rect { fill: var(--accent-primary); }
	rect[data-empty='true'] { fill: var(--border); }
	.cap { display: flex; justify-content: space-between; margin-top: 4px; font-size: 11px; color: var(--text-muted); }
	/* One initial under each bar. */
	.cap.each span { flex: 1; text-align: center; }
	.note { margin: var(--space-3) 0 0; font-size: 13px; color: var(--text-secondary); }
</style>
