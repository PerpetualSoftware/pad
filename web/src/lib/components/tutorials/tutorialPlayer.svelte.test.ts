// TASK-3452: the in-app player is a facade: no iframe (so nothing from
// YouTube) until the viewer presses play, then a youtube-nocookie iframe.
import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import TutorialPlayer from './TutorialPlayer.svelte';

describe('TutorialPlayer', () => {
	it('mounts no iframe before play, and a youtube-nocookie one after', async () => {
		const { container } = render(TutorialPlayer, { videoId: 'abc123', title: 'T', poster: '/p.jpg' });
		expect(container.querySelector('iframe')).toBeNull();
		await fireEvent.click(screen.getByRole('button', { name: 'Play video: T' }));
		const src = container.querySelector('iframe')?.getAttribute('src') ?? '';
		expect(src.startsWith('https://www.youtube-nocookie.com/embed/abc123?')).toBe(true);
	});

	it('with no video id yet shows "coming soon" and no play button', () => {
		render(TutorialPlayer, { videoId: null, title: 'T', poster: '/p.jpg' });
		expect(screen.getByText('Video coming soon')).toBeTruthy();
		expect(screen.queryByRole('button')).toBeNull();
	});
});
