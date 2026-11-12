<script lang="ts">
	import { deadlineTone, longDay, roleDay } from '$lib/calendar/presentation';
	import type { AgendaEntry } from '$lib/types';

	interface Props {
		/** The days shown, in order. */
		days: string[];
		today: string;
		/** The day the list below is showing, when it shows one day. */
		selected?: string;
		/** Every entry in the window; each day counts its own. */
		entries: AgendaEntry[];
		/** How much of a day `entries` holds. A day not loaded shows no count; one partly loaded shows no total. */
		loadState?: (day: string) => 'full' | 'partial' | 'none';
		zone: string;
		now?: Date;
		/** Phone layout: the cells are narrow, so each shows fewer pips. */
		compact?: boolean;
		/** The occurrence being dragged. While set, every day is a drop target. */
		dragging: AgendaEntry | null;
		/** The day the drag is over. */
		hoverDay: string;
		onselect: (day: string) => void;
		onenter: (day: string) => void;
		ondrop: (day: string) => void;
	}

	let { days, today, selected, entries, loadState = () => 'full', zone, now = new Date(Date.now()), compact = false, dragging, hoverDay, onselect, onenter, ondrop }: Props = $props();

	type Tone = 'bad' | 'owed' | 'quiet' | 'done';
	const ORDER: Tone[] = ['bad', 'owed', 'quiet', 'done'];
	// Most severe first: when a day has more entries than pips, the pips left out are the mildest.
	const MAX_PIPS = 6;
	const MAX_PIPS_COMPACT = 4;
	let root = $state<HTMLDivElement>();
	let focused = $state('');

	function tone(entry: AgendaEntry): Tone {
		if (entry.type === 'date') return entry.overdue ? 'bad' : 'quiet';
		if (entry.state === 'lapsed' || entry.state === 'missed') return 'bad';
		if (entry.state === 'outstanding') {
			const deadline = entry.roles?.deadline;
			return entry.overdue || (deadline && deadlineTone(deadline, zone, now) === 'passed') ? 'bad' : 'owed';
		}
		return entry.state === 'done' ? 'done' : 'quiet';
	}
	const load = $derived(new Map(days.map(day => {
		const state = loadState(day);
		const rows = state === 'none' ? [] : entries.filter(entry => entry.day === day && (entry.type === 'schedule' || entry.type === 'date'));
		const tones = rows.map(tone).sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b));
		const owed = rows.filter(entry => entry.type === 'schedule' && (entry.state === 'outstanding' || entry.state === 'lapsed' || entry.state === 'missed')).length;
		return [day, { state, count: rows.length, owed, tones: tones.slice(0, compact ? MAX_PIPS_COMPACT : MAX_PIPS) }] as const;
	})));
	// One tab stop: the day last focused, else the selected day, else today, else the first.
	const stop = $derived([focused, selected, today].find(day => day && days.includes(day)) ?? days[0]);
	const weekday = (day: string) => new Intl.DateTimeFormat('en-GB', { weekday: 'short', timeZone: 'UTC' }).format(new Date(`${day}T12:00Z`));
	const pin = (day: string) => !dragging ? '' : dragging.roles?.deadline && roleDay(dragging.roles.deadline, zone) === day ? 'Deadline'
		: dragging.roles?.earliest && roleDay(dragging.roles.earliest, zone) === day ? 'Available from' : '';

	function label(day: string): string {
		const { state, count, owed } = load.get(day)!;
		const name = [longDay(day), ...(day === today ? ['Today'] : [])];
		// Only a fully loaded day has a total, or can be called empty.
		if (state !== 'full') return [...name, state === 'none' ? 'not loaded' : 'not fully loaded'].join(', ');
		return [...name, count ? `${count} ${count === 1 ? 'entry' : 'entries'}` : 'no entries', ...(owed ? [`${owed} owed`] : [])].join(', ');
	}
	function figure(day: string): string | number {
		const { state, count, owed } = load.get(day)!;
		if (state !== 'full') return state === 'partial' && count ? `${count}+` : '';
		return count === 0 ? '–' : day === today && owed ? `${owed} owed` : count;
	}
	function onkeydown(event: KeyboardEvent, index: number) {
		if (event.altKey || event.ctrlKey || event.metaKey) return;
		const rtl = root ? getComputedStyle(root).direction === 'rtl' : false;
		const step = (delta: number) => (index + delta + days.length) % days.length;
		let next: number;
		switch (event.key) {
			case 'ArrowRight': next = step(rtl ? -1 : 1); break;
			case 'ArrowLeft': next = step(rtl ? 1 : -1); break;
			case 'Home': next = 0; break;
			case 'End': next = days.length - 1; break;
			default: return;
		}
		event.preventDefault();
		focused = days[next];
		root?.querySelector<HTMLButtonElement>(`[data-strip-day="${days[next]}"]`)?.focus();
	}
</script>

<div bind:this={root} class="strip" role="group" aria-label="Days">
	{#each days as day, index (day)}
		{@const info = load.get(day)!}
		{@const pinned = pin(day)}
		{@const over = !!dragging && hoverDay === day}
		<button type="button" class="day" class:today={day === today} class:quiet={info.state === 'full' && info.count === 0} class:over class:pinned={pinned === 'Deadline'}
			data-strip-day={day} data-load={info.state === 'full' ? undefined : info.state} data-drop={dragging ? 'true' : undefined}
			aria-label={label(day)} aria-current={day === today ? 'date' : undefined} aria-pressed={selected ? day === selected : undefined}
			tabindex={day === stop ? 0 : -1}
			onclick={() => onselect(day)} onfocus={() => focused = day} onkeydown={event => onkeydown(event, index)}
			ondragenter={event => { if (dragging) { event.preventDefault(); onenter(day); } }}
			ondragover={event => { if (dragging) { event.preventDefault(); if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'; } }}
			ondrop={event => { event.preventDefault(); ondrop(day); }}>
			<span class="date"><span class="dow">{weekday(day)}</span><span class="num">{+day.slice(8)}</span></span>
			<span class="load">
				<span class="n">{over && !pinned ? 'Move here' : pinned || figure(day)}</span>
				<span class="pips">{#each info.tones as pip, at (at)}<span class="pip" data-tone={pip}></span>{/each}</span>
			</span>
		</button>
	{/each}
</div>

<style>
	.strip { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); gap: var(--space-2); margin-top: var(--space-4); }
	.day { display: flex; align-items: center; gap: var(--space-3); min-width: 0; min-height: 52px; padding: 8px 12px; overflow: hidden; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-secondary); color: var(--text-primary); font: inherit; text-align: left; cursor: pointer; }
	.day:hover { background: var(--bg-hover); }
	.day:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 2px; }
	.date { display: flex; flex-direction: column; }
	.dow { font-size: 11px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase; color: var(--text-muted); }
	.num { font-size: 18px; font-weight: 700; line-height: 1.1; font-variant-numeric: tabular-nums; }
	.load { margin-left: auto; display: flex; flex-direction: column; align-items: flex-end; gap: 3px; min-width: 0; max-width: 100%; overflow: hidden; font-size: 11px; color: var(--text-muted); font-variant-numeric: tabular-nums; }
	.n { white-space: nowrap; }
	.pips { display: flex; gap: 3px; min-height: 6px; max-width: 100%; overflow: hidden; }
	.pip { flex: 0 0 6px; width: 6px; height: 6px; border-radius: 50%; background: var(--border-strong); }
	.pip[data-tone='owed'] { background: var(--accent-amber); }
	.pip[data-tone='bad'] { background: var(--accent-red); }
	.pip[data-tone='done'] { background: var(--accent-green); }
	.today { border-color: var(--accent-primary); box-shadow: inset 0 0 0 1px var(--accent-primary); }
	.today .num { color: var(--accent-primary); }
	.quiet { background: transparent; }
	.day[aria-pressed='true'] { background: var(--bg-tertiary); border-color: var(--border-strong); }
	.day[data-drop] { border-style: dashed; }
	.over { border-color: var(--accent-primary); background: color-mix(in srgb, var(--accent-primary) 10%, var(--bg-secondary)); }
	.over .n { color: var(--accent-primary); font-weight: 600; }
	.pinned { border-color: var(--accent-red); }
	.pinned .n { color: var(--accent-red); font-weight: 600; }
	@media (max-width: 1099px) {
		.day:not([data-drop]) .n { display: none; }
	}
	@media (max-width: 768px) {
		.strip { gap: 4px; margin-top: var(--space-3); }
		.day { flex-direction: column; align-items: center; gap: 1px; min-height: 58px; padding: 6px 0 7px; }
		.date { align-items: center; }
		.num { font-size: 16px; }
		.load { margin: 2px 0 0; align-items: center; }
		.day .n { display: none; }
	}
</style>
