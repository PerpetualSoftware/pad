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

// TASK-4101 PR2a r1 H-L2: explicit zone writes (settings Save, notice Switch) bump
// this counter before their PATCH. The browser default adopts its response only
// if no explicit write began after it, so a reordered late default answer cannot
// overwrite a zone the user chose a moment later.
let explicitWrites = 0;
export function noteExplicitTimezoneWrite(): void { explicitWrites++; }
export function explicitTimezoneWrites(): number { return explicitWrites; }
