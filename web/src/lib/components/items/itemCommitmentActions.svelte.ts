// The item page's occurrence actions (TASK-4600): Done, Skip, Snooze, Move,
// Bring to today and Set forecast on the commitment's current occurrence.
//
// A narrow copy of the Agenda flows (routes/(console)/agenda/+page.svelte:
// action, resolve, chooseEnd, loadTitles, moveError, commitTarget, moveTarget,
// cancelReview), so the Agenda page stays byte-identical. Left out on purpose:
// undo (readUndo), the lapsed-history Mark done, the drag cache and the day
// dialog. The Agenda's reload becomes deps.refresh(), which re-reads only the
// commitment. Copy strings are the Agenda's, verbatim.
//
// Every await is fenced: the account identity (authStore.identityFence), the
// item identity captured when the flow starts, and disposal. A flow whose
// fence fails writes nothing more and publishes nothing.
import { tick } from 'svelte';
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { canAcknowledge, canMove, canResolve, dateTime, entryKey, isAllDay, outstandingMatches, sameTarget, targetOnDay, todayIn, type AgendaAction } from '$lib/calendar/presentation';
import type { ScheduleEndChoice } from '$lib/components/calendar/ScheduleEndDialog.svelte';
import type { AgendaEntry, CommitmentMoveResult, ItemStillOpenDetails, ScheduleResolveState } from '$lib/types';

export const staleCopy = 'This occurrence changed. Review the updated entry and try again.';
export const permissionCopy = 'You can no longer change this item.';
const genericCopy = 'Could not update this occurrence. Try again.';
const notFiredCopy = 'This occurrence has not fired yet, so there is nothing to acknowledge.';

export interface ItemCommitmentActionDeps {
	identity: () => { wsSlug: string; itemSlug: string; itemId: string };
	zone: () => string;
	/** Re-read the commitment. false: the read failed. */
	refresh: () => Promise<boolean>;
	/** Where the moved occurrence's actions button lives, and the focus fallback. */
	root?: () => HTMLElement | undefined;
}

type Review = { entry: AgendaEntry; to: string; preview: CommitmentMoveResult; reason?: string; sameIdentity: () => boolean };

export class ItemCommitmentActions {
	pending = $state<Record<string, boolean>>({});
	saved = $state<Record<string, boolean>>({});
	dialogMode = $state<'snooze' | 'move'>('snooze');
	dialogEntry = $state<AgendaEntry | null>(null);
	forecastEntry = $state<AgendaEntry | null>(null);
	endChoice = $state<{ entry: AgendaEntry; details: ItemStillOpenDetails; comment?: string } | null>(null);
	endError = $state('');
	review = $state<Review | null>(null);
	committing = $state(false);
	actionError = $state('');
	announcement = $state('');
	linkedTitles = $state<Record<string, string>>({});
	refreshFailed = $state(false);
	// Plain edge counter, never read by a render: bumping it fails every open fence.
	#epoch = 0;
	#deps: ItemCommitmentActionDeps;

	constructor(deps: ItemCommitmentActionDeps) { this.#deps = deps; }

	#fence(): () => boolean {
		const account = authStore.identityFence();
		const epoch = this.#epoch;
		const { wsSlug, itemSlug, itemId } = this.#deps.identity();
		return () => {
			if (!account() || epoch !== this.#epoch) return false;
			const now = this.#deps.identity();
			return now.wsSlug === wsSlug && now.itemSlug === itemSlug && now.itemId === itemId;
		};
	}
	#succeed(message: string) { this.announcement = message; }
	#unchanged(entry: AgendaEntry, to: string) { return sameTarget(entry.roles?.target ?? entry.at, to, this.#deps.zone()); }

	/** Drop every flow in flight and every open dialog: the item or account changed. */
	reset = () => {
		this.#epoch++;
		this.pending = {}; this.saved = {}; this.dialogEntry = null; this.forecastEntry = null; this.endChoice = null; this.endError = '';
		this.review = null; this.committing = false; this.actionError = ''; this.announcement = ''; this.refreshFailed = false; this.linkedTitles = {};
	};
	dispose = () => { this.#epoch++; };

	action = (entry: AgendaEntry, state: AgendaAction) => {
		if (this.pending[entryKey(entry)] || this.saved[entryKey(entry)] || !canResolve(entry)) return;
		this.actionError = '';
		if (state === 'forecast') { this.forecastEntry = entry; return; }
		if (state === 'today') { if (canMove(entry)) void this.moveTarget(entry, targetOnDay(entry, todayIn(this.#deps.zone()), this.#deps.zone())); return; }
		if (state === 'move' && !canMove(entry)) return;
		if (state === 'ack' && !canAcknowledge(entry)) return;
		if (state === 'move' || state === 'snooze') { this.dialogMode = state; this.dialogEntry = entry; }
		else void this.resolve(entry, state);
	};
	submitDialog = (until: string, comment?: string) => {
		const entry = this.dialogEntry;
		if (!entry) return;
		if (this.dialogMode === 'move') void this.moveTarget(entry, until, comment || undefined);
		else void this.resolve(entry, 'snooze', until, comment);
	};
	closeDialog = () => { this.dialogEntry = null; };
	closeEnd = () => { this.endChoice = null; this.endError = ''; };
	closeForecast = () => { this.forecastEntry = null; };
	forecastSaved = () => { this.forecastEntry = null; void this.#deps.refresh(); };
	retryRefresh = async () => {
		const sameIdentity = this.#fence();
		const refreshed = await this.#deps.refresh();
		if (sameIdentity() && refreshed) this.refreshFailed = false;
	};

	// end: the caller confirmed ending the schedule of an open item. closeAs then closes the item with that value.
	resolve = async (entry: AgendaEntry, state: Exclude<ScheduleResolveState, 'move'>, until?: string, comment?: string, end?: { closeAs?: string }): Promise<boolean> => {
		const key = entryKey(entry);
		if (this.pending[key] || this.saved[key] || !canResolve(entry)) return false;
		const sameIdentity = this.#fence();
		this.pending = { ...this.pending, [key]: true };
		this.actionError = ''; this.announcement = '';
		let viewing = true;
		try {
			// The outstanding occurrence is named by the instant the schedule holds now.
			const view = await api.getCommitment(entry.workspace!, entry.item_ref);
			if (!sameIdentity()) return false;
			if (!outstandingMatches(entry, view) || !view.next_occurrence) {
				this.actionError = staleCopy;
				await this.#deps.refresh();
				return false;
			}
			const occurrenceAt = view.next_occurrence.at;
			viewing = false;
			const resolved = await api.resolveCommitment(entry.workspace!, entry.item_ref, {
				occurrence_at: occurrenceAt, state,
				...(until !== undefined ? { until } : {}),
				...(comment !== undefined ? { comment } : {}),
				...(end ? { end_schedule: true } : {})
			});
			if (!sameIdentity()) return false;
			this.saved = { ...this.saved, [key]: true };
			if (this.dialogEntry && entryKey(this.dialogEntry) === key) this.dialogEntry = null;
			// end_schedule has no effect when a concurrent edit gave the schedule another
			// occurrence: only a resolution that ended it with the item open closes the item.
			const ended = resolved?.resolution?.item_still_open === true;
			// The schedule has ended; closing the item is a second write that can be refused on its own.
			let closeError = '';
			let itemClosed = false;
			if (ended && end?.closeAs !== undefined && this.endChoice) {
				try {
					await api.items.update(entry.workspace!, entry.item_ref, { fields_patch: { [this.endChoice.details.done_field]: end.closeAs } });
					itemClosed = true;
				} catch (error) {
					const message = (error as { message?: unknown })?.message;
					closeError = `The schedule has ended, but closing the item failed: ${typeof message === 'string' && message ? message : 'try again'}. It stays open in Outstanding.`;
				}
				if (!sameIdentity()) return false;
			}
			const refreshed = await this.#deps.refresh();
			if (!sameIdentity()) return false;
			if (closeError) this.actionError = closeError;
			if (refreshed === false) this.refreshFailed = true;
			const verb = state === 'skip' ? 'Occurrence skipped' : 'Occurrence marked done';
			if (end && !ended && !resolved?.commitment?.ended_at) {
				this.#succeed(end.closeAs !== undefined ? `${verb}. The schedule continues, so the item was not closed.` : `${verb}. The schedule continues.`);
			} else if (end && !ended) {
				// The schedule ended, but not with the item open: it was closed concurrently,
				// or its collection can no longer close it.
				this.#succeed(`${verb}; the schedule has ended.`);
			} else if (end) {
				this.#succeed(itemClosed ? `${verb}; the schedule has ended and the item is closed.` : `${verb}; the schedule has ended. The item stays open in Outstanding.`);
			} else {
				this.#succeed({ done: 'Occurrence marked done.', skip: 'Occurrence skipped.', snooze: 'Occurrence snoozed.', ack: 'Acknowledged. The work stays outstanding.' }[state]);
			}
			return true;
		} catch (error) {
			if (!sameIdentity()) return false;
			const code = (error as { code?: string })?.code;
			// The last occurrence of an open item: nothing was written. Ask how to end it.
			if (code === 'item_still_open' && (state === 'done' || state === 'skip') && !end) {
				const details = (error as { details?: Partial<ItemStillOpenDetails> })?.details ?? {};
				this.endError = '';
				this.endChoice = { entry, comment, details: { state, occurrence_at: details.occurrence_at ?? '', done_field: details.done_field || 'status', item_status: details.item_status ?? '', close_options: details.close_options ?? [], ack_available: !!details.ack_available } };
				return false;
			}
			const stale = code === 'stale' || (viewing && code === 'not_found');
			const notFired = state === 'ack' && code === 'not_fired';
			const forbidden = code === 'forbidden' || code === 'permission_denied';
			const message = (error as { message?: unknown })?.message;
			// The server's own sentence says who may skip before a hard deadline; the entry is unchanged, so no reload.
			const ownerRequired = code === 'deadline_owner_required' && typeof message === 'string' && message !== '';
			this.actionError = ownerRequired ? message as string : stale ? staleCopy : notFired ? notFiredCopy : forbidden ? permissionCopy : genericCopy;
			if (stale || notFired || forbidden) await this.#deps.refresh();
			return false;
		} finally {
			if (sameIdentity()) this.pending = { ...this.pending, [key]: false };
		}
	};
	chooseEnd = async (choice: ScheduleEndChoice) => {
		const asked = this.endChoice;
		if (!asked) return;
		const { entry, details, comment } = asked;
		this.endError = '';
		const done = choice.kind === 'ack'
			? await this.resolve(entry, 'ack')
			: await this.resolve(entry, details.state, undefined, comment, choice.kind === 'close' ? { closeAs: choice.value } : {});
		if (this.endChoice !== asked) return;
		if (done || this.actionError === staleCopy) this.endChoice = null;
		else { this.endError = this.actionError || genericCopy; this.actionError = ''; }
	};
	loadTitles = async (entry: AgendaEntry, preview: CommitmentMoveResult) => {
		const sameIdentity = this.#fence();
		const ids = [...new Set([...preview.changes, ...(preview.violations ?? []), ...(preview.left_in_place ?? [])].filter(change => !change.root && !(change.item_id in this.linkedTitles)).map(change => change.item_id))];
		if (!ids.length) return;
		const titles = await Promise.all(ids.map(async id => {
			try { const item = await api.items.get(entry.workspace!, id); return [id, item.title || 'Linked item'] as const; }
			catch { return [id, 'Linked item'] as const; }
		}));
		if (sameIdentity()) this.linkedTitles = { ...this.linkedTitles, ...Object.fromEntries(titles) };
	};
	#moveError = async (error: unknown) => {
		const code = (error as { code?: string })?.code;
		const stale = code === 'commitment_changed' || code === 'stale';
		const forbidden = code === 'forbidden' || code === 'permission_denied' || code === 'link_edit_required';
		const message = (error as { message?: unknown })?.message;
		const serverMessage = ['deadline_approval_required', 'deadline_owner_required', 'schedule_linked', 'target_after_deadline'].includes(code ?? '') && typeof message === 'string' && message.trim();
		this.actionError = stale ? staleCopy : forbidden ? permissionCopy : serverMessage ? message as string : genericCopy;
		if (stale || forbidden) { this.dialogEntry = null; this.review = null; await this.#deps.refresh(); }
	};
	#focusMoved = async () => {
		await tick();
		const root = this.#deps.root?.();
		const button = root?.querySelector<HTMLButtonElement>('button[data-agenda-actions]');
		(button && !button.disabled ? button : root)?.focus();
	};
	commitTarget = async (entry: AgendaEntry, to: string, preview: CommitmentMoveResult, sameIdentity: () => boolean, reason?: string, acknowledge?: boolean, confirmPastDeadline?: boolean) => {
		if (!sameIdentity()) return;
		const key = entryKey(entry);
		const reviewed = this.review && entryKey(this.review.entry) === key;
		if (reviewed) this.committing = true;
		try {
			await api.moveCommitment(entry.workspace!, entry.item_ref, { role: 'target', to, revision: preview.revision, ...(reason ? { reason } : {}), ...(acknowledge ? { acknowledge_violations: true } : {}), ...(preview.past_deadline && confirmPastDeadline ? { confirm_past_deadline: true } : {}) });
			if (!sameIdentity()) return;
			this.saved = { ...this.saved, [key]: true };
			if (this.dialogEntry && entryKey(this.dialogEntry) === key) this.dialogEntry = null;
			if (this.review && entryKey(this.review.entry) === key) this.review = null;
			const refreshed = await this.#deps.refresh();
			if (!sameIdentity()) return;
			if (refreshed === false) this.refreshFailed = true;
			this.#succeed(`Occurrence moved to ${dateTime(to, this.#deps.zone(), isAllDay(entry))}. Its reminder follows the target.`);
			this.pending = { ...this.pending, [key]: false };
			await this.#focusMoved();
		} catch (error) {
			if (sameIdentity()) {
				await this.#moveError(error);
				if ((error as { code?: string })?.code === 'target_after_deadline') {
					try {
						const fresh = await api.previewCommitmentMove(entry.workspace!, entry.item_ref, { role: 'target', to, ...(reason ? { reason } : {}) });
						if (!sameIdentity()) return;
						this.dialogEntry = null;
						this.review = { entry, to, preview: fresh, reason, sameIdentity };
						void this.loadTitles(entry, fresh);
					} catch (previewError) { if (sameIdentity()) await this.#moveError(previewError); }
				}
			}
		} finally {
			if (sameIdentity()) { if (reviewed) this.committing = false; this.pending = { ...this.pending, [key]: false }; }
		}
	};
	moveTarget = async (entry: AgendaEntry, to: string, reason?: string) => {
		const key = entryKey(entry);
		if (!canMove(entry) || this.pending[key] || this.saved[key] || this.review) return;
		if (this.#unchanged(entry, to)) { if (this.dialogEntry && entryKey(this.dialogEntry) === key) this.dialogEntry = null; return; }
		const sameIdentity = this.#fence();
		this.pending = { ...this.pending, [key]: true }; this.actionError = ''; this.announcement = '';
		let reviewing = false;
		try {
			const preview = await api.previewCommitmentMove(entry.workspace!, entry.item_ref, { role: 'target', to, ...(reason ? { reason } : {}) });
			if (!sameIdentity()) return;
			const root = preview.changes.find(change => change.root);
			if (!root?.from || !this.#unchanged(entry, root.from)) {
				this.actionError = staleCopy; this.dialogEntry = null; await this.#deps.refresh(); return;
			}
			if (preview.renegotiates_deadline || preview.violations?.length || preview.past_deadline) {
				this.dialogEntry = null;
				this.review = { entry, to, preview, reason, sameIdentity }; reviewing = true;
				void this.loadTitles(entry, preview);
			} else await this.commitTarget(entry, to, preview, sameIdentity, reason);
		} catch (error) {
			if (sameIdentity()) await this.#moveError(error);
		} finally {
			if (sameIdentity() && !reviewing) this.pending = { ...this.pending, [key]: false };
		}
	};
	commitReview = async (reason?: string, acknowledge?: boolean, confirmPastDeadline?: boolean) => {
		const review = this.review;
		if (!review || this.committing) return;
		await this.commitTarget(review.entry, review.to, review.preview, review.sameIdentity, reason, acknowledge, confirmPastDeadline);
	};
	cancelReview = () => {
		if (!this.review || this.committing) return;
		this.pending = { ...this.pending, [entryKey(this.review.entry)]: false }; this.review = null; this.actionError = '';
	};
}

export function createItemCommitmentActions(deps: ItemCommitmentActionDeps): ItemCommitmentActions {
	return new ItemCommitmentActions(deps);
}
