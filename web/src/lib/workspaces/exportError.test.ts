import { describe, it, expect } from 'vitest';
import { exportErrorMessage, formatBytes } from './exportError';

// TASK-4845: the server's export_too_large message names CLI flags. The web
// page composes its own wording from the refusal's details.

const MiB = 1024 * 1024;

describe('formatBytes', () => {
	it('uses binary units', () => {
		expect(formatBytes(512)).toBe('512 B');
		expect(formatBytes(1536)).toBe('1.5 KiB');
		expect(formatBytes(64 * MiB)).toBe('64 MiB');
		expect(formatBytes(2.25 * 1024 * MiB)).toBe('2.3 GiB');
	});
});

describe('exportErrorMessage', () => {
	it('size limit with history: suggests leaving out version history, then the operator setting', () => {
		const msg = exportErrorMessage({
			code: 'export_too_large',
			message: 'pad workspace export --no-versions',
			details: { size_reached: 70 * MiB, limit_bytes: 64 * MiB, include_versions: true }
		});
		expect(msg).toContain('64 MiB');
		expect(msg).toContain('70 MiB');
		expect(msg).toContain('Leave out version history');
		expect(msg).toContain('PAD_EXPORT_METADATA_MAX_BYTES');
		expect(msg).not.toContain('--no-versions');
	});

	it('size limit without history: only the operator setting', () => {
		const msg = exportErrorMessage({
			code: 'export_too_large',
			message: 'server words',
			details: { size_reached: 70 * MiB, limit_bytes: 64 * MiB, include_versions: false }
		});
		expect(msg).toContain('even without version history');
		expect(msg).toContain('64 MiB');
		expect(msg).toContain('PAD_EXPORT_METADATA_MAX_BYTES');
		expect(msg).not.toContain('Leave out version history" and try');
	});

	it('entry-count limit: too many attachments, history does not help, names the CLI JSON export', () => {
		const msg = exportErrorMessage({
			code: 'export_too_large',
			message: 'server words',
			details: { entries: 120_001, limit_entries: 100_000, header_blocks: 10, limit_header_blocks: 1_000_000 }
		});
		expect(msg).toContain('too many attachments');
		expect(msg).toContain('does not help');
		expect(msg).toContain('pad workspace export --json');
		expect(msg).not.toContain('PAD_EXPORT_METADATA_MAX_BYTES');
	});

	it('unknown details shape: falls back to the server message', () => {
		expect(
			exportErrorMessage({ code: 'export_too_large', message: 'server words', details: { other: 1 } })
		).toBe('server words');
		expect(exportErrorMessage({ code: 'export_too_large', message: 'server words' })).toBe('server words');
	});

	it('other errors: the envelope message, or a generic one', () => {
		expect(exportErrorMessage({ code: 'forbidden', message: 'No access' })).toBe('No access');
		expect(exportErrorMessage({ code: 'x', message: '' })).toBe('Export failed. Try again.');
		expect(exportErrorMessage(new TypeError('boom'))).toBe('Export failed. Try again.');
	});
});
