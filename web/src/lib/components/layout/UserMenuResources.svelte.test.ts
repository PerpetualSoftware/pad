// @vitest-environment jsdom
//
// TASK-2888: the Community link (the repo's GitHub Discussions tab) is a
// project-wide channel, so it must appear in BOTH the Cloud and the
// self-hosted Resources lists, sit immediately after GitHub (docs/brand.md
// §7 canonical order — a surface may omit links but never reorders the
// ones it keeps), and take its href from the shared brand module rather
// than a re-typed literal.
import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import UserMenuResources from './UserMenuResources.svelte';
import AuthFooter from '../auth/AuthFooter.svelte';
import { COMMUNITY_URL, GITHUB_REPO_URL } from '$lib/brand/links';

afterEach(() => cleanup());

function labels(container: HTMLElement): (string | undefined)[] {
	return Array.from(container.querySelectorAll('a')).map((a) => a.textContent?.trim());
}

function communityLink(container: HTMLElement): HTMLAnchorElement | null {
	return (
		Array.from(container.querySelectorAll('a')).find(
			(a) => a.textContent?.trim() === 'Community'
		) ?? null
	);
}

describe('Community link (TASK-2888)', () => {
	it('points at the repo Discussions tab, derived from the repo URL', () => {
		expect(COMMUNITY_URL).toBe(`${GITHUB_REPO_URL}/discussions`);
	});

	it.each([true, false])('user menu renders Community (cloudMode=%s), external', (cloudMode) => {
		const { container } = render(UserMenuResources, { props: { cloudMode } });
		const a = communityLink(container);
		expect(a, 'Community link missing').not.toBeNull();
		expect(a?.getAttribute('href')).toBe(COMMUNITY_URL);
		expect(a?.getAttribute('target')).toBe('_blank');
		expect(a?.getAttribute('rel')).toBe('noopener noreferrer');
	});

	it.each([true, false])(
		'user menu places Community immediately after GitHub (cloudMode=%s)',
		(cloudMode) => {
			const { container } = render(UserMenuResources, { props: { cloudMode } });
			const order = labels(container);
			expect(order.indexOf('GitHub')).toBeGreaterThanOrEqual(0);
			expect(order.indexOf('Community')).toBe(order.indexOf('GitHub') + 1);
		}
	);

	it('auth footer renders Community on Cloud, immediately after GitHub', () => {
		const { container } = render(AuthFooter, { props: { cloudMode: true } });
		const order = labels(container);
		expect(order.indexOf('GitHub')).toBeGreaterThanOrEqual(0);
		expect(order.indexOf('Community')).toBe(order.indexOf('GitHub') + 1);
		expect(communityLink(container)?.getAttribute('href')).toBe(COMMUNITY_URL);
	});

	it('auth footer renders nothing on self-hosted (unchanged)', () => {
		const { container } = render(AuthFooter, { props: { cloudMode: false } });
		expect(container.querySelector('a')).toBeNull();
	});
});
