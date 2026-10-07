// TASK-3452: the first-time tutorial card on Pad Cloud. Its own file because
// the tutorials store is a module singleton (catalog and dismissals load once).
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';

const dismiss = vi.fn(async (key: string) => ({ dismissed: [key] }));
vi.mock('$lib/api/client', () => ({
	api: {
		tutorials: {
			list: vi.fn(async () => ({
				available: true,
				paths: [],
				demos: [],
				tutorials: [{ slug: 'new-project', title: 'Start a new project with Pad', youtube_id: null, seconds: 149, poster: '/api/v1/tutorials/posters/new-project', chapters: [] }]
			}))
		},
		uiDismissals: { list: vi.fn(async () => ({ dismissed: [] })), dismiss }
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { cloudMode: true } }));

const TutorialSuggestion = (await import('./TutorialSuggestion.svelte')).default;

describe('TutorialSuggestion on Pad Cloud', () => {
	it('shows the same-origin poster and links to the in-app player, then hides for good on dismiss', async () => {
		const { container } = render(TutorialSuggestion, { dismissKey: 'tutorials.console', slug: 'new-project', fallbackTitle: 'fallback' });
		const link = await screen.findByRole('link', { name: /Start a new project with Pad/ });
		expect(link.getAttribute('href')).toBe('/console/tutorials/new-project');
		expect(link.getAttribute('target')).toBeNull();
		const img = container.querySelector('img');
		expect(img?.getAttribute('src')).toBe('/api/v1/tutorials/posters/new-project');
		expect(screen.getByText('2:29')).toBeTruthy();

		await fireEvent.click(screen.getByRole('button', { name: 'Dismiss this suggestion' }));
		expect(dismiss).toHaveBeenCalledWith('tutorials.console');
		await waitFor(() => expect(screen.queryByRole('link', { name: /Start a new project/ })).toBeNull());
	});
});
