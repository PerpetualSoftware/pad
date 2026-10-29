export function browserZone(): string | null {
	try { return Intl.DateTimeFormat().resolvedOptions().timeZone || null; }
	catch { return null; }
}

export function sameZone(a: string, b: string): boolean {
	try {
		const canonical = (zone: string) => new Intl.DateTimeFormat('en-US', { timeZone: zone }).resolvedOptions().timeZone;
		return canonical(a) === canonical(b);
	} catch { return a === b; }
}

export function zoneOptions(): string[] {
	const fallback = ['America/Los_Angeles', 'America/New_York', 'Europe/London', 'Europe/Paris', 'Asia/Kolkata', 'Asia/Tokyo', 'Australia/Sydney'];
	try { return ['UTC', ...Intl.supportedValuesOf('timeZone')]; }
	catch { return ['UTC', ...fallback]; }
}
