// TASK-3612: the two entry points to the move-to-Cloud guide. Each shows on
// its own side only, and neither shows inside a mobile app's web view
// (TASK-3299; lead ruling, day 92).
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';

const auth = vi.hoisted(() => ({ authStore: { cloudMode: false, nativeShell: false } }));
vi.mock('$lib/stores/auth.svelte', () => auth);

const MoveToCloudLink = (await import('./MoveToCloudLink.svelte')).default;

function guideLinks(container: HTMLElement): Element[] {
	return [...container.querySelectorAll('a')].filter(
		(a) => a.getAttribute('href') === '/console/move-to-cloud'
	);
}

describe('MoveToCloudLink', () => {
	beforeEach(() => {
		auth.authStore.cloudMode = false;
		auth.authStore.nativeShell = false;
	});

	it('shows the settings entry on a self-hosted instance in a browser (control)', () => {
		const { container } = render(MoveToCloudLink, { on: 'self-hosted' });
		expect(guideLinks(container)).toHaveLength(1);
	});

	it('shows the import entry on Cloud in a browser (control)', () => {
		auth.authStore.cloudMode = true;
		const { container } = render(MoveToCloudLink, { on: 'cloud' });
		expect(guideLinks(container)).toHaveLength(1);
	});

	it('shows neither entry on the other side', () => {
		const selfHostedImport = render(MoveToCloudLink, { on: 'cloud' });
		expect(guideLinks(selfHostedImport.container)).toHaveLength(0);
		auth.authStore.cloudMode = true;
		const cloudSettings = render(MoveToCloudLink, { on: 'self-hosted' });
		expect(guideLinks(cloudSettings.container)).toHaveLength(0);
	});

	it('shows neither entry inside a mobile app', () => {
		auth.authStore.nativeShell = true;
		const settings = render(MoveToCloudLink, { on: 'self-hosted' });
		expect(settings.container.textContent?.trim()).toBe('');
		auth.authStore.cloudMode = true;
		const importTab = render(MoveToCloudLink, { on: 'cloud' });
		expect(importTab.container.textContent?.trim()).toBe('');
	});

	it('calls onnavigate when the link is followed', () => {
		const onnavigate = vi.fn();
		const { container } = render(MoveToCloudLink, { on: 'self-hosted', onnavigate });
		(guideLinks(container)[0] as HTMLAnchorElement).addEventListener('click', (e) => e.preventDefault());
		(guideLinks(container)[0] as HTMLAnchorElement).click();
		expect(onnavigate).toHaveBeenCalledOnce();
	});
});
