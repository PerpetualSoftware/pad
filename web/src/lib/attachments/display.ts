/**
 * Display helpers shared by every attachment surface (TASK-2383).
 *
 * These were private to `$lib/components/settings/StorageTab.svelte` until the
 * item attachment strip needed the same mime table; extracted here verbatim so
 * there is exactly one icon mapping and one byte formatter to maintain.
 */

// Same algorithm as web/src/routes/console/billing/+page.svelte. Picks a
// unit so the displayed value is < 1024; bump thresholds nudged down half
// the previous unit so 1,048,575 bytes reads as "1.0 MB" rather than the
// misleading "1024 KB" you'd get from a straight Math.round at the KB tier.
export function formatBytes(bytes: number): string {
	if (bytes < 0) return `${bytes} B`;
	const KB = 1024;
	const MB = KB * 1024;
	const GB = MB * 1024;
	const bumpGB = GB - MB / 2;
	const bumpMB = MB - KB / 2;
	if (bytes >= bumpGB) return formatUnit(bytes / GB, 'GB');
	if (bytes >= bumpMB) return formatUnit(bytes / MB, 'MB');
	if (bytes >= KB) return formatUnit(bytes / KB, 'KB');
	return `${bytes} B`;
}

function formatUnit(value: number, unit: string): string {
	if (value >= 10) return `${Math.round(value)} ${unit}`;
	return `${value.toFixed(1)} ${unit}`;
}

export function categoryIcon(mime: string): string {
	if (mime.startsWith('image/')) return '🖼️';
	if (mime.startsWith('video/')) return '🎬';
	if (mime.startsWith('audio/')) return '🔊';
	if (mime.startsWith('text/')) return '📄';
	if (mime === 'application/pdf') return '📄';
	if (
		mime === 'application/zip' ||
		mime === 'application/x-tar' ||
		mime === 'application/gzip' ||
		mime === 'application/x-7z-compressed' ||
		mime === 'application/x-rar-compressed'
	)
		return '📦';
	if (
		mime.startsWith('application/vnd.openxmlformats') ||
		mime.startsWith('application/vnd.ms-') ||
		mime.startsWith('application/vnd.oasis') ||
		mime === 'application/msword'
	)
		return '📄';
	return '❓';
}

export function isImage(mime: string): boolean {
	return mime.startsWith('image/');
}
