import { describe, expect, it } from 'vitest';
import type { Collection } from '$lib/types';
import { artifactCollectionSlug } from './artifactSlug';

function c(slug: string, traits?: unknown): Collection {
	return { id: slug, slug, traits: traits === undefined ? undefined : JSON.stringify(traits) } as unknown as Collection;
}

describe('artifactCollectionSlug (BUG-3481)', () => {
	it('finds the collection by the trait, whatever its slug', () => {
		const cs = [c('tasks'), c('rules', { artifact_kind: { kind: 'convention' } }), c('procedures', { artifact_kind: { kind: 'playbook' } })];
		expect(artifactCollectionSlug(cs, 'convention')).toBe('rules');
		expect(artifactCollectionSlug(cs, 'playbook')).toBe('procedures');
	});

	it('is null when nothing declares the kind, or traits are malformed', () => {
		expect(artifactCollectionSlug([c('conventions'), c('x', 'not-an-object')], 'convention')).toBeNull();
		expect(artifactCollectionSlug([{ id: 'y', slug: 'y', traits: '{bad' } as unknown as Collection], 'convention')).toBeNull();
	});
});
