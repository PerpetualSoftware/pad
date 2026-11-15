import type { AgendaEntry } from '$lib/types';
import type { ToastAction } from '$lib/stores/toast.svelte';

// Only the agenda provides this context; shared calendar rows elsewhere stay unchanged.
export const agendaUndoContext = 'agendaUndo';
export type AgendaUndo = (entry: AgendaEntry) => Promise<ToastAction | undefined>;

export function hasUndoMenu(entry: AgendaEntry): boolean {
 return entry.type === 'schedule' && !!entry.occurrence_key &&
  ['done', 'skipped', 'moved', 'outstanding'].includes(entry.state ?? '');
}

const resolutionLabels: Record<string, string> = { done: 'Done', skip: 'Skip', snooze: 'Snooze', move: 'Move' };

export function resolutionLabel(state: unknown): string | undefined {
 return typeof state === 'string' && Object.hasOwn(resolutionLabels, state) ? resolutionLabels[state] : undefined;
}
