/**
 * The item pane's save indicator, derived from the writes actually OUTSTANDING
 * rather than from whichever write reached a shared variable last (BUG-3044).
 *
 * `status` is:
 *   - 'saving' while ANY begun write has not settled;
 *   - 'saved' (for SAVED_MS) once the last one settles, iff the NEWEST-ISSUED
 *     write succeeded — an older success cannot announce a value that a newer
 *     write has since failed to store;
 *   - 'idle' otherwise.
 *
 * A single last-writer value got three sequences wrong: an older write's success
 * showed "Saved" while a newer write was still in flight; one field's failure
 * showed idle while another field was still saving; and a previous "Saved"
 * timer fired mid-request, because starting a save never cleared it. The pane's
 * SSE gates read `status === 'saving'`, so each of those also admitted an
 * incoming event while a local write was outstanding.
 *
 * Every `begin()` must be `settle()`d, on EVERY return path — including a write
 * that was superseded by a newer one for the same field, which otherwise never
 * decrements and pins 'saving' (and every SSE gate) until navigation. Call
 * `settle` from a `finally`. A token from before the last `reset()` (the pane
 * switched items) is ignored, so a late settle cannot move the next item's
 * indicator. Settling a token twice is a no-op.
 */

export type SaveStatus = 'idle' | 'saving' | 'saved';

/** How long "Saved" stays up before the indicator goes idle. */
export const SAVED_MS = 2000;

export interface SaveToken {
	readonly epoch: number;
	readonly n: number;
	ok: boolean;
	settled: boolean;
}

export class SaveTracker {
	status = $state<SaveStatus>('idle');

	#epoch = 0;
	#outstanding = 0;
	#issued = 0;
	#newestOk = false;
	#timer: ReturnType<typeof setTimeout> | undefined;

	/** A write is starting. Clears any pending "Saved" timer. */
	begin(): SaveToken {
		this.#clearTimer();
		this.#outstanding += 1;
		this.#issued += 1;
		this.status = 'saving';
		return { epoch: this.#epoch, n: this.#issued, ok: false, settled: false };
	}

	/** Record that this write succeeded; it still has to be settled. */
	succeed(token: SaveToken): void {
		token.ok = true;
	}

	/** This write is over, whatever happened to it. Call from a `finally`. */
	settle(token: SaveToken): void {
		if (token.settled) return;
		token.settled = true;
		if (token.epoch !== this.#epoch) return;
		this.#outstanding -= 1;
		if (token.n === this.#issued) this.#newestOk = token.ok;
		if (this.#outstanding > 0) return;
		if (this.#newestOk) {
			this.status = 'saved';
			this.#timer = setTimeout(() => {
				this.#timer = undefined;
				this.status = 'idle';
			}, SAVED_MS);
		} else {
			this.status = 'idle';
		}
	}

	/** Forget every outstanding write (the pane moved to another item). */
	reset(): void {
		this.#epoch += 1;
		this.#outstanding = 0;
		this.#issued = 0;
		this.#newestOk = false;
		this.#clearTimer();
		this.status = 'idle';
	}

	/** Stop the timer; for component teardown. */
	destroy(): void {
		this.#clearTimer();
	}

	#clearTimer(): void {
		clearTimeout(this.#timer);
		this.#timer = undefined;
	}
}
