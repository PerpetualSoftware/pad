// Web wording for a refused workspace bundle export (TASK-4845). The server's
// export_too_large message names CLI flags, which mean nothing in the
// browser, so the settings page composes its own sentence from the refusal's
// details. Two detail shapes exist (internal/server/export_spool.go
// writeExportTooLarge, internal/server/export_entry_limit.go
// writeExportBundleEntriesTooLarge); anything else falls back to the
// server's message.

const GENERIC = 'Export failed. Try again.';
const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];

export function formatBytes(n: number): string {
	let v = n;
	let i = 0;
	while (v >= 1024 && i < UNITS.length - 1) {
		v /= 1024;
		i++;
	}
	if (i === 0) return `${Math.round(v)} B`;
	const s = v.toFixed(1).replace(/\.0$/, '');
	return `${s} ${UNITS[i]}`;
}

function num(v: unknown): v is number {
	return typeof v === 'number' && Number.isFinite(v);
}

export function exportErrorMessage(err: unknown): string {
	const e = (err ?? {}) as { code?: unknown; message?: unknown; details?: unknown };
	const message = typeof e.message === 'string' && e.message.trim() !== '' ? e.message : '';
	if (typeof e.code !== 'string') return GENERIC;
	if (e.code !== 'export_too_large') return message || GENERIC;

	const d = (e.details && typeof e.details === 'object' ? e.details : {}) as Record<string, unknown>;

	if (num(d.limit_bytes) && num(d.size_reached) && typeof d.include_versions === 'boolean') {
		const limit = formatBytes(d.limit_bytes);
		const reached = formatBytes(d.size_reached);
		// The server stops spooling just past the limit, so the two often
		// round to the same figure.
		const stopped = reached === limit ? 'it stopped just past that' : `it stopped at ${reached}`;
		if (d.include_versions) {
			return (
				`This export is larger than this server's limit of ${limit} (${stopped}). ` +
				`Tick "Leave out version history" and try again; that usually fits. ` +
				`Otherwise the server operator can raise PAD_EXPORT_METADATA_MAX_BYTES.`
			);
		}
		return (
			`This export is larger than this server's limit of ${limit} even without version history (${stopped}). ` +
			`The server operator can raise PAD_EXPORT_METADATA_MAX_BYTES.`
		);
	}

	if (num(d.entries) && num(d.limit_entries)) {
		return (
			`This workspace has too many attachments for a bundle this server can import. ` +
			`Leaving out version history does not help. ` +
			`The CLI command pad workspace export --json exports the workspace without attachment blobs.`
		);
	}

	return message || GENERIC;
}
