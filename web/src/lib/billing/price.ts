// Formatting a price the sidecar serves (TASK-3577). Stripe reports amounts in
// the currency's minor unit (cents for usd, whole yen for jpy), so the number
// of fraction digits comes from the currency, not a fixed 100.

/** `unitAmount` (minor units) of `currency` as display text: "$8", "$7.50", "¥800". */
export function formatPrice(unitAmount: number, currency: string): string {
	const code = currency.trim().toUpperCase();
	try {
		const digits = new Intl.NumberFormat('en-US', { style: 'currency', currency: code }).resolvedOptions()
			.maximumFractionDigits ?? 2;
		const amount = unitAmount / 10 ** digits;
		const whole = Number.isInteger(amount);
		return new Intl.NumberFormat('en-US', {
			style: 'currency',
			currency: code,
			minimumFractionDigits: whole ? 0 : digits,
			maximumFractionDigits: whole ? 0 : digits,
		}).format(amount);
	} catch {
		// An unknown currency code: say what it is rather than guess a symbol.
		return `${(unitAmount / 100).toFixed(2)} ${code}`;
	}
}

/** "/ month" or "/ year" for a served interval. */
export function perInterval(interval: string): string {
	return interval === 'annual' ? '/ year' : '/ month';
}
