// TASK-2203 (audit C46): what a page shows when its load FAILED, as against
// when it succeeded and found nothing. BUG-2025 gave the dashboard, insights
// and graph an error card with a retry; these pages rendered a failure as their
// empty state ("No tags yet", "No members yet."), which is confidently wrong.
//
// A refusal (403) is not retryable and gets a permission sentence instead; any
// other failure gets the server's message when it sent one, and a retry.
export interface LoadFailure {
	title: string;
	detail: string;
	/** False for a permission refusal: asking again gets the same answer. */
	retryable: boolean;
}

export function loadFailure(what: string, err: unknown): LoadFailure {
	// By its code rather than `instanceof PadApiError`: the code is the API's
	// contract, and a page whose client module is substituted still answers.
	if ((err as { code?: unknown } | null)?.code === 'forbidden') {
		return {
			title: `You don't have access to ${what}`,
			detail: 'Ask an owner of this workspace if you need it.',
			retryable: false,
		};
	}
	const message = err instanceof Error && err.message ? err.message : '';
	return {
		title: `Couldn't load ${what}`,
		detail: message || 'It may be a temporary network or server issue.',
		retryable: true,
	};
}
