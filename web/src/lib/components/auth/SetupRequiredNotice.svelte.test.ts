import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import SetupRequiredNotice from './SetupRequiredNotice.svelte';

afterEach(cleanup);

it('explains startup-link recovery and keeps the paste-token setup path (BUG-4334)', () => {
	const { container } = render(SetupRequiredNotice, { setupMethod: 'logs_token' });

	expect(screen.getByRole('heading', { name: 'Finish setting up Pad' })).toBeInTheDocument();
	expect(screen.getByText('This Pad instance has not been initialized yet.')).toBeInTheDocument();
	expect(container.querySelector('.body')).toHaveTextContent(
		'Open the setup link printed when Pad started. If you started Pad in a terminal, look there. For Docker, run docker logs <container> and find “Pad first-run setup”.'
	);
	expect(container.querySelector('code')).toHaveTextContent(/^docker logs <container>$/);
	expect(container).not.toHaveTextContent('grep');
	expect(container).not.toHaveTextContent('grab it from your container logs');
	expect(screen.getByText('Then continue setup')).toBeInTheDocument();
	expect(screen.getByRole('link', { name: 'Open setup page' })).toHaveAttribute('href', '/setup');
});
