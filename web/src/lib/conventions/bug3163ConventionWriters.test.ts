import { describe, it, expect } from 'vitest';
import { conventionCreatePayload } from './createPayload';

// BUG-3163: create's `fields` refuses every reserved metadata key, so the web
// convention writer that builds a create must send the metadata as the typed
// `convention` member and keep it OUT of `fields`. A writer left on the old shape is refused 400
// by the server, which is the regression each test here would catch.

const metadata = {
	category: 'quality',
	trigger: 'on-commit',
	surfaces: ['all'],
	enforcement: 'must' as const,
	commands: ['make test']
};

describe('convention writers send the typed member (BUG-3163)', () => {
	// api.library.activate no longer sends fields at all: since TASK-3462 the
	// server builds the item (bug3446WebActivateShape.test.ts pins the body).

	it('the Conventions page payload carries `convention` beside `fields`, not inside it', () => {
		const payload = conventionCreatePayload('Run tests', 'body', metadata);

		expect(payload.convention).toEqual(metadata);
		const fields = JSON.parse(String(payload.fields));
		expect(fields).not.toHaveProperty('convention');
		expect(fields).toMatchObject({ status: 'active', trigger: 'on-commit', scope: 'all', priority: 'must' });
	});
});
