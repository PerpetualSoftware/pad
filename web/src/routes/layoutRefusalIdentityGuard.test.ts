// Node-project SOURCE guard: the root layout records a workspace refusal against
// the identity that ASKED, not whoever is signed in when the answer lands
// (BUG-2983, codex round 1 P1).
//
// Source rather than render: the registration happens at module scope in a
// layout that pulls in the whole app shell, and the property is structural —
// the regression is a future edit dropping the comparison and calling
// `markUnreachable` with the current user, which shows up in the source and
// would not show up in any component suite. Same posture as
// `itemDetailUsesPicker.test.ts`.
//
// WHAT A SOURCE GUARD CANNOT DO: it checks spellings, not behaviour. The
// behaviour it stands for — the stamp itself — is measured in
// `lib/api/workspaceGoneSeam.test.ts`, which switches identity mid-flight and
// asserts the scope still carries the original.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const source = readFileSync(fileURLToPath(new URL('./+layout.svelte', import.meta.url)), 'utf8');

// Comments quote the very identifiers asserted below, so strip them or the
// guard passes on its own documentation.
const code = source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|\s)\/\/[^\n]*/g, '$1');

describe('the root layout', () => {
	it('registers the identity provider the API client stamps requests with', () => {
		expect(code).toContain('setIdentityProvider(');
	});

	it('compares the stamped identity before recording a refusal', () => {
		expect(code).toContain('scope.identity');
		// The early return is the guard. Without it the comparison would be
		// computed and ignored — which is how this defect would come back.
		expect(code).toMatch(/if \(issuedAs !== undefined && issuedAs !== now\) return;/);
	});

	it('records the refusal through markUnreachable, not a bare reset', () => {
		// A bare `reset` leaves the next bootstrap cold, which refetches, which
		// 404s, which resets: the loop this fix closes, reopened through its own
		// cleanup.
		expect(code).toContain('localIndex.markUnreachable(');
		expect(code).not.toMatch(/setAccessRevokedHandler\(\(scope\) => \{\s*localIndex\.reset\(/);
	});
});
