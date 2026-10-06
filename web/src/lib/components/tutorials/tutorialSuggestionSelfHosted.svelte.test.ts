// TASK-3452: the first-time tutorial card on a self-hosted instance links out
// to getpad.dev/learn, never asks the server for a catalog, and requests no
// image. Its own file: the tutorials store is a module singleton.
import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';

const list = vi.fn();
vi.mock('$lib/api/client', () => ({
	api: {
		tutorials: { list },
		uiDismissals: { list: vi.fn(async () => ({ dismissed: ['tutorials.console'] })), dismiss: vi.fn() }
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { cloudMode: false } }));

const TutorialSuggestion = (await import('./TutorialSuggestion.svelte')).default;

describe('TutorialSuggestion on a self-hosted instance', () => {
	it('links to getpad.dev/learn with no catalog fetch and no image', async () => {
		const { container } = render(TutorialSuggestion, { dismissKey: 'tutorials.launchpad', slug: 'onboard', fallbackTitle: 'Set up your workspace with /pad onboard' });
		const link = await screen.findByRole('link', { name: /Set up your workspace with \/pad onboard/ });
		expect(link.getAttribute('href')).toBe('https://getpad.dev/learn/onboard');
		expect(link.getAttribute('target')).toBe('_blank');
		expect(container.querySelector('img')).toBeNull();
		expect(list).not.toHaveBeenCalled();
	});

	it('renders nothing for a suggestion the user dismissed (on any device)', async () => {
		// The control: a key the user did NOT dismiss appears, which proves the
		// dismissals have loaded before the absence below is read.
		render(TutorialSuggestion, { dismissKey: 'tutorials.launchpad', slug: 'onboard', fallbackTitle: 'Control tutorial' });
		render(TutorialSuggestion, { dismissKey: 'tutorials.console', slug: 'new-project', fallbackTitle: 'Dismissed tutorial' });
		await screen.findByRole('link', { name: /Control tutorial/ });
		expect(screen.queryByRole('link', { name: /Dismissed tutorial/ })).toBeNull();
	});
});
