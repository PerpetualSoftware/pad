import type { AgendaEntry, AgendaWorkEntry } from '$lib/types';
import { attentionEntries } from './presentation';

export type EntryClass = 'mine' | 'agents';
export type AudienceView = EntryClass | 'all';
export type AudienceCounts = Record<AudienceView, number>;

export function classify(entry: Pick<AgendaEntry, 'type' | 'audience'>): EntryClass {
	return entry.type === 'schedule' && entry.audience === 'agent' ? 'agents' : 'mine';
}

export function defaultAudience(isAgent: boolean): EntryClass {
	return isAgent ? 'agents' : 'mine';
}

export function parseAudience(value: string | null | undefined, isAgent: boolean): AudienceView {
	return value === 'mine' || value === 'agents' || value === 'all' ? value : defaultAudience(isAgent);
}

export function audienceChip(entryClass: EntryClass, view: AudienceView, isAgent: boolean): 'agents' | 'you' | null {
	const implied = view === 'all' ? defaultAudience(isAgent) : view;
	return entryClass === implied ? null : entryClass === 'agents' ? 'agents' : 'you';
}

export function filterAudience(entries: AgendaEntry[], view: AudienceView): AgendaEntry[] {
	return entries.filter(entry => view === 'all' || classify(entry) === view);
}

export function audienceCounts(entries: AgendaEntry[]): AudienceCounts {
	const counts = { mine: 0, agents: 0, all: 0 };
	for (const entry of entries) {
		if ((entry.type === 'schedule' || entry.type === 'date') && entry.state !== 'done') counts[classify(entry)]++;
	}
	counts.all = counts.mine + counts.agents;
	return counts;
}

/** Attention is derived before filtering so every view keeps the same obligations visible. */
export function splitAudience(entries: AgendaEntry[], priority: AgendaWorkEntry[], view: AudienceView) {
	return { entries: filterAudience(entries, view), attention: attentionEntries(entries, priority) };
}
