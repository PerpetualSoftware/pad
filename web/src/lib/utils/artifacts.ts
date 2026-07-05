// Artifact export/import helpers shared across the conventions + playbook
// surfaces (Phase 4 web UI). Centralizes the browser-only Blob/object-URL
// download dance and the file-read-then-import flow so each page only wires
// up its own buttons + toasts.

import { api } from '$lib/api/client';
import type { ImportArtifactResult } from '$lib/types';

/**
 * Trigger a client-side download of `text` as `filename` using a Blob +
 * object URL + a synthetic `<a download>` click, revoking the URL afterward.
 * No-op outside the browser (guards against SSR).
 */
export function downloadTextFile(filename: string, text: string, mime = 'text/markdown'): void {
	if (typeof document === 'undefined' || typeof URL === 'undefined') return;
	const blob = new Blob([text], { type: mime });
	const url = URL.createObjectURL(blob);
	const a = document.createElement('a');
	a.href = url;
	a.download = filename;
	document.body.appendChild(a);
	a.click();
	document.body.removeChild(a);
	URL.revokeObjectURL(url);
}

/**
 * Export an item as a `.pad.md` artifact and immediately download it. Throws
 * the underlying PadApiError so callers can surface `err.message` in a toast.
 */
export async function exportAndDownloadArtifact(ws: string, ref: string): Promise<void> {
	const { filename, text } = await api.exportItemArtifact(ws, ref);
	downloadTextFile(filename, text);
}

/**
 * Export the current user's account data as a JSON artifact and immediately
 * download it (TASK-1960). Throws the underlying PadApiError so callers can
 * surface `err.message` in a toast — e.g. the restricted-owner 403 (BUG-1945).
 */
export async function exportAndDownloadAccountData(): Promise<void> {
	const { filename, text } = await api.auth.exportAccountData();
	downloadTextFile(filename, text, 'application/json');
}

/**
 * Read a picked artifact File as text and POST it to the import endpoint.
 * Returns the server result (ref + slug + warnings); throws on read failure
 * or a 4xx import error (PadApiError) for the caller to surface.
 */
export async function importArtifactFile(
	ws: string,
	file: File
): Promise<ImportArtifactResult> {
	const text = await file.text();
	return api.importArtifact(ws, text);
}
