import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { IMPORT_FINISHING_MS, IMPORT_STALL_MS } from './importUpload';

// BUG-3475 (lead ruling Q3): the client's bounds are constants, pinned here
// against the server's per-Read idle window so the two cannot drift apart
// silently. The stall bound must not exceed the window (the client never
// waits longer for progress than the server would wait for bytes); the
// finishing bound must exceed it (a server that is still finishing, which can
// itself spend up to a window on its last read, is given that time).
//
// Read from the Go source, because that constant is the server's truth and no
// endpoint publishes it (no capabilities plumbing, by the same ruling).

function serverIdleMs(): number {
	const src = readFileSync(resolve(process.cwd(), '../internal/server/import_read_deadline.go'), 'utf8');
	const m = src.match(/defaultImportReadIdle\s*=\s*(\d+)\s*\*\s*time\.(Second|Minute)/);
	if (!m) throw new Error('defaultImportReadIdle not found in import_read_deadline.go: update this pin with it');
	return Number(m[1]) * (m[2] === 'Minute' ? 60_000 : 1_000);
}

describe('import upload bounds against the server idle window', () => {
	it('stall <= idle < finishing', () => {
		const idle = serverIdleMs();
		expect(idle).toBe(60_000);
		expect(IMPORT_STALL_MS).toBeLessThanOrEqual(idle);
		expect(IMPORT_FINISHING_MS).toBeGreaterThan(idle);
	});
});
