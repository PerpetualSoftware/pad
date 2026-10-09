<script lang="ts">
	import PasswordRuleHint from '$lib/components/auth/PasswordRuleHint.svelte';
	import { isPasswordRuleError, localPasswordProblem, passwordDescribedBy } from '$lib/auth/passwordRule';
	import { untrack } from 'svelte';
	import { goto, replaceState } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type InvitationPreview } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';
	import AuthOAuthButtons from '$lib/components/auth/AuthOAuthButtons.svelte';
	import { recordAuthMethod, getLastAuthMethod, type AuthMethod } from '$lib/auth/lastMethod';
	import { validateRedirect } from '$lib/auth/redirect';
	import { captureInvitationProof, clearInvitationProof } from '$lib/invitations/proof';
	import { autofocus } from '$lib/a11y/autofocus';

	let code = $derived(page.params.code ?? '');
	// TASK-3352: the mailbox-only proof from the invitation EMAIL's link
	// (`#proof=…`). Sent with the accept or the signup, it verifies the
	// address; the code alone never does. Captured on mount, kept for this tab
	// across a sign-in round trip, and stripped from the address bar.
	let proof = $state('');
	// OAuth completes outside the SPA and returns via a full-page navigation to
	// this same /join/<code> URL, where onMount's session probe sees
	// `authenticated` and shows the accept/decline card (BUG-2136). Thread the code through the
	// SSO link's ?redirect= so that round trip lands back here to finish
	// accepting the invite (BUG-1931 / DR-8). validateRedirect keeps this to a
	// same-origin relative path — no open redirect.
	let oauthRedirectTarget = $derived(validateRedirect(`/join/${code}`));
	let status = $state<
		'loading' | 'login' | 'register' | 'confirm' | 'accepting' | 'declining' | 'declined' | 'not-joined' | 'error' | 'setup' | '2fa' | 'invalid'
	>('loading');
	// The invited workspace's name, from the preview, for the accept/decline
	// card (BUG-2136 U2).
	let invitedWorkspaceName = $state('');
	let errorMsg = $state('');
	const announcement = $derived(
		status === 'accepting'
			? 'Joining workspace...'
			: status === 'declining'
				? 'Declining invitation...'
				: status === 'declined'
					? 'Invitation declined. You were not added to the workspace.'
					: ''
	);
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | 'logs_token' | 'open' | undefined>(undefined);

	// Auth form state
	let mode = $state<'login' | 'register'>('register');
	let email = $state('');
	// When the non-consuming preview (BUG-1934) resolves the invited address,
	// we prefill `email` and lock the field: the backend binds the invite to
	// this exact address (register 403s on mismatch; accept requires the
	// signed-in account to match), so letting the invitee retype it only
	// invites the confusing invitation_email_mismatch rejection this fixes.
	let invitedEmail = $state<string | null>(null);
	let name = $state('');
	let username = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let formError = $state('');
	// A refusal for a new password itself, shown on the field (TASK-2260).
	let passwordError = $state('');
	let submitting = $state(false);
	let challengeToken = $state('');
	let totpCode = $state('');

	// Last-used auth method (github/google/password) → drives the "Last used"
	// pill on the matching OAuth button, mirroring /login and /register.
	let lastMethod = $state<AuthMethod | null>(null);

	let usernameManuallyEdited = $state(false);
	let usernameChecking = $state(false);
	let usernameAvailable = $state<boolean | null>(null);
	let usernameError = $state('');
	let checkTimeout: ReturnType<typeof setTimeout> | null = null;

	// The code the page is showing. SvelteKit keeps this component when only
	// the code changes (an SPA navigation from one /join link to another), so
	// the flow restarts for each code, every action addresses the code whose
	// invitation is on screen, and an answer for an earlier code is dropped
	// (BUG-2136 U2, codex r4). Every restart bumps flowSeq, and every await
	// below checks it, so nothing an earlier flow started (a preview, a session
	// probe, an accept, a decline, a sign-in) lands on the current one (codex r5).
	let flowCode = '';
	let flowSeq = 0;
	const flowFence = () => {
		const mine = flowSeq;
		return () => mine === flowSeq;
	};
	$effect(() => {
		const c = code;
		if (c !== untrack(() => flowCode)) untrack(() => void start(c));
	});

	async function start(c: string) {
		flowCode = c;
		flowSeq++;
		const current = flowFence();
		status = 'loading';
		invitedWorkspaceName = '';
		invitedEmail = null;
		email = '';
		mode = 'register';
		errorMsg = '';
		formError = '';
		challengeToken = '';
		// Nothing typed for the previous invitation is carried to this one
		// (codex r6). `submitting` is left alone: a sign-in still out keeps the
		// form busy until it answers, so a second one cannot overlap it.
		name = '';
		username = '';
		password = '';
		confirmPassword = '';
		passwordError = '';
		totpCode = '';
		usernameManuallyEdited = false;
		if (checkTimeout) clearTimeout(checkTimeout);
		checkTimeout = null;
		usernameChecking = false;
		usernameAvailable = null;
		usernameError = '';
		proof = captureInvitationProof(c, window.location.hash);
		if (window.location.hash) {
			replaceState(window.location.pathname + window.location.search, page.state);
		}

		// Read the last-used auth method so the OAuth buttons can paint the
		// "Last used" pill on first render. Fails silent in SSR / private mode.
		const last = getLastAuthMethod();
		if (last) lastMethod = last.method;

		// Hydrate authStore so AuthHeader can branch on cloudMode (matching the
		// pattern in /forgot-password and /reset-password). Fire-and-forget so
		// it does not delay the join-flow logic below — the header just won't
		// render its Cloud branch on the very first paint if the session
		// fetch is mid-flight, and it lights up once authStore resolves.
		authStore.ensureLoaded().catch(() => {});

		// Kick off the non-consuming invitation preview (BUG-1934) in parallel
		// with the session probe. It's always-200 and never accepts the invite;
		// it just tells us the invited email (to prefill read-only) and whether
		// an account already exists (to pick login vs register). Best-effort: on
		// failure we fall back to a blank, editable email field.
		const previewPromise = api.members.previewInvitation(c).catch(() => null);

		try {
			const session = await api.auth.session();
			if (!current()) return;
			if (session.authenticated) {
				// Already signed in: ASK (BUG-2136, lead ruling). Opening the link
				// is not consent; the card offers accept or decline.
				if (!(await applyPreview(previewPromise, current))) return;
				status = 'confirm';
				return;
			}
			if (session.setup_required) {
				// Ahead of the preview on purpose (TASK-2251, codex r1): an
				// instance with no admin yet holds no invitations, so the
				// preview could only say found:false, and "an admin must finish
				// setup" is the message that can actually be acted on.
				setupMethod = session.setup_method;
				status = 'setup';
				return;
			}
			// Logged out — show the auth form.
			if (!(await applyPreview(previewPromise, current))) return;
			status = 'login';
		} catch {
			// Session probe itself failed — still show the form (defaulting to
			// register unless the preview says an account exists). See BUG-1930.
			if (!current()) return;
			if (!(await applyPreview(previewPromise, current))) return;
			status = 'login';
		}
	}

	// Apply the invitation preview to the auth form: prefill + lock the invited
	// email, and default the mode by whether an account already exists. When
	// the preview REQUEST failed we keep the register default: a
	// never-registered invitee has no account to sign into, and register passes
	// the code to auto-accept in one step (BUG-1930). The "already have an
	// account? sign in" switch still lets a returning user flip to login.
	//
	// A preview that ANSWERED found:false is different (TASK-2251): the server
	// says no live invitation behind this code (unknown, expired, or its
	// workspace deleted; it does not say which, by design). Offering a
	// register form there sends someone through signup only to have the join
	// fail at the end, so the page says the link is invalid instead.
	//
	// Answers false, applying nothing, when the page has moved on to another
	// code, or when it has shown the invalid state; either way the caller stops.
	async function applyPreview(
		previewPromise: Promise<InvitationPreview | null>,
		current: () => boolean
	): Promise<boolean> {
		const preview = await previewPromise;
		if (!current()) return false;
		if (preview && !preview.found) {
			status = 'invalid';
			return false;
		}
		if (preview?.found && preview.workspace_name) invitedWorkspaceName = preview.workspace_name;
		if (preview?.found && preview.email) {
			email = preview.email;
			invitedEmail = preview.email;
			mode = preview.has_account ? 'login' : 'register';
		} else {
			mode = 'register';
		}
		return true;
	}

	// OAuth completes entirely outside the SPA (provider → pad-cloud callback →
	// server-set session cookie → server-driven redirect back to /join/<code>),
	// so there's no JS success callback to hang the record on. Mirror
	// /login and /register: record speculatively on click. See lastMethod.ts.
	function handleOAuthClick(provider: AuthMethod) {
		recordAuthMethod(provider);
	}

	// Land IN the workspace just joined (TASK-3279, PLAN-3002 Q5), after an
	// accept or a signup with this invitation code (BUG-3284); the workspace
	// layout opens it as an ephemeral tab. The list is reloaded first so the
	// new workspace is in it when the layout resolves it. A server that
	// predates the slug in the response lands on /console.
	//
	// The session is re-read first, as /login does after signing in: a signup
	// or sign-in from this page leaves the client's auth state saying "signed
	// out", and the landing, the tab bar and the workspace list all wait on
	// it. A tab that was already signed in as the same user re-reads the same
	// session, which notifies nobody.
	async function landInJoinedWorkspace(
		joined: { workspace_slug?: string; owner_username?: string } | undefined,
		current: () => boolean
	) {
		const session = await authStore.load().catch(() => null);
		// load() joins a session read already in flight, and one sent before
		// this page's sign-in set the cookie answers "signed out" (codex r1).
		// A second read, sent now, carries the new session.
		if (!session?.authenticated) await authStore.load().catch(() => {});
		await workspaceStore.loadAll().catch(() => {});
		const dest =
			joined?.workspace_slug && joined.owner_username
				? `/${encodeURIComponent(joined.owner_username)}/${encodeURIComponent(joined.workspace_slug)}`
				: '/console';
		if (!current()) return;
		await goto(dest, { replaceState: true });
	}

	async function acceptInvitation() {
		const c = flowCode;
		const current = flowFence();
		status = 'accepting';
		try {
			const result = await api.members.acceptInvitation(c, proof || undefined);
			// The "+" badge must stop counting it, and the throttle would hold a
			// navigation's refetch back (BUG-2136 U2). Whatever page is showing now.
			void pendingInvitations.refresh(true);
			// A's proof is spent whichever page is showing now; the landing is
			// fenced, so a moved-on page is not navigated away from.
			clearInvitationProof(c);
			await landInJoinedWorkspace(result, current);
		} catch (err: unknown) {
			if (!current()) return;
			errorMsg = err instanceof Error ? err.message : 'Failed to accept invitation';
			status = 'error';
		}
	}

	// Decline from the link (BUG-2136): deletes the invitation; nothing joins.
	async function declineInvitation() {
		const c = flowCode;
		const current = flowFence();
		status = 'declining';
		try {
			await api.members.declineInvitation(c);
			void pendingInvitations.refresh(true);
			clearInvitationProof(c);
			if (!current()) return;
			status = 'declined';
		} catch (err: unknown) {
			if (!current()) return;
			errorMsg = err instanceof Error ? err.message : 'Failed to decline invitation';
			status = 'error';
		}
	}

	function generateUsername(name: string): string {
		let u = name.toLowerCase().trim();
		u = u.replace(/[^a-z0-9-]+/g, '-');
		u = u.replace(/-{2,}/g, '-');
		u = u.replace(/^-|-$/g, '');
		if (u.length > 39) u = u.substring(0, 39).replace(/-$/, '');
		return u;
	}

	function handleNameInput() {
		if (!usernameManuallyEdited) {
			username = generateUsername(name);
			checkUsernameAvailability();
		}
	}

	function handleUsernameInput() {
		usernameManuallyEdited = username !== '' && username !== generateUsername(name);
		checkUsernameAvailability();
	}

	function checkUsernameAvailability() {
		if (checkTimeout) clearTimeout(checkTimeout);
		usernameAvailable = null;
		usernameError = '';

		if (!username || username.length < 3) {
			usernameChecking = false;
			return;
		}

		usernameChecking = true;
		const current = flowFence();
		checkTimeout = setTimeout(async () => {
			try {
				const result = await api.auth.checkUsername(username);
				if (!current()) return;
				usernameAvailable = result.available;
				usernameError = result.message || '';
			} catch {
				if (!current()) return;
				usernameError = '';
				usernameAvailable = null;
			} finally {
				if (current()) usernameChecking = false;
			}
		}, 400);
	}

	// A sign-in or signup that completes after the code changed belongs to the
	// earlier flow: the current one re-reads the session, which now carries the
	// new sign-in, rather than taking this one's result (codex r5).
	function restartIfMovedOn(current: () => boolean): boolean {
		if (current()) return false;
		submitting = false;
		void start(flowCode);
		return true;
	}

	async function handleSubmit() {
		formError = '';
		passwordError = '';
		submitting = true;
		const c = flowCode;
		const current = flowFence();

		try {
			if (mode === 'register') {
				if (!name.trim()) { formError = 'Name is required'; submitting = false; return; }
				if (username && username.length < 3) { formError = 'Username must be at least 3 characters'; submitting = false; return; }
				if (usernameAvailable === false) { formError = usernameError || 'Username is not available'; submitting = false; return; }
				if (!email.trim()) { formError = 'Email is required'; submitting = false; return; }
				passwordError = localPasswordProblem(password) ?? '';
				if (passwordError) { submitting = false; return; }
				if (password !== confirmPassword) { formError = 'Passwords do not match'; submitting = false; return; }
				// Pass the invitation code so the backend allows registration
				// and auto-accepts the invitation in one step.
				const registered = await api.auth.register(
					email.trim(),
					name.trim(),
					password,
					username || undefined,
					c,
					proof || undefined
				);
				void pendingInvitations.refresh(true);
				// Spent whichever page is showing now.
				clearInvitationProof(c);
				if (restartIfMovedOn(current)) return;
				// Registration with invitation_code already accepted the invite,
				// so land directly instead of calling acceptInvitation().
				// Kept without its invitation (BUG-3438): registration was open
				// anyway, so the account exists, but the invitation was gone or
				// could not be applied. Say so, and land nowhere.
				if (registered.invitation_not_joined) {
					await authStore.load().catch(() => {});
					if (!current()) return;
					submitting = false;
					errorMsg = registered.invitation_not_joined.message;
					status = 'not-joined';
					return;
				}
				await landInJoinedWorkspace(registered.accepted_invitation, current);
				return;
			} else {
				if (!email.trim()) { formError = 'Email is required'; submitting = false; return; }
				if (!password) { formError = 'Password is required'; submitting = false; return; }
				const response = await api.auth.login(email.trim(), password);
				if (restartIfMovedOn(current)) return;

				if (response.requires_2fa && response.challenge_token) {
					challengeToken = response.challenge_token;
					status = '2fa';
					submitting = false;
					return;
				}
			}
			// Signed in from the link: ask, as for a visitor who was already
			// signed in (BUG-2136). Only REGISTERING through the code joins in one
			// step, because creating the account here is the intent to join.
			submitting = false;
			status = 'confirm';
		} catch (err: unknown) {
			if (!current()) {
				submitting = false;
				return;
			}
			if (mode === 'register' && err instanceof Error && isPasswordRuleError(err.message)) {
				passwordError = err.message;
			} else {
				formError = err instanceof Error ? err.message : 'Authentication failed';
			}
			submitting = false;
		}
	}

	async function handleVerify2FA() {
		formError = '';
		const code = totpCode.trim();

		if (!code) {
			formError = 'Please enter your authentication code.';
			return;
		}

		submitting = true;
		const current = flowFence();
		try {
			const isTotp = /^\d{6}$/.test(code);

			if (isTotp) {
				await api.auth.verify2FA(challengeToken, code, undefined);
			} else {
				await api.auth.verify2FA(challengeToken, undefined, code);
			}

			if (restartIfMovedOn(current)) return;
			// 2FA verified: ask, as above (BUG-2136).
			submitting = false;
			status = 'confirm';
		} catch (err: unknown) {
			if (!current()) {
				submitting = false;
				return;
			}
			formError = err instanceof Error ? err.message : 'Invalid code. Please try again.';
			submitting = false;
		}
	}

	function handleBack2FA() {
		status = 'login';
		challengeToken = '';
		totpCode = '';
		formError = '';
		submitting = false;
	}
</script>

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="join-page" class:cloud-mode={authStore.cloudMode}>
	<div class="join-card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

		<!-- Accept and Decline remove the focused buttons, so their progress and
		     result are spoken from a region that stays mounted (codex r4). -->
		<p class="sr-only" role="status" aria-live="polite">{announcement}</p>
		{#if status === 'loading'}
			<p class="subtitle">Checking invitation...</p>
		{:else if status === 'confirm'}
			<p class="subtitle">
				You've been invited to join
				{#if invitedWorkspaceName}<strong>{invitedWorkspaceName}</strong>{:else}a workspace{/if}.
			</p>
			<div class="form confirm-actions">
				<button onclick={acceptInvitation} data-testid="join-accept">Accept invitation</button>
				<button class="secondary-button" onclick={declineInvitation} data-testid="join-decline" type="button">Decline</button>
			</div>
		{:else if status === 'accepting'}
			<p class="subtitle">Joining workspace...</p>
		{:else if status === 'declining'}
			<p class="subtitle">Declining invitation...</p>
		{:else if status === 'declined'}
			<p class="subtitle">Invitation declined. You were not added to the workspace.</p>
			<a href="/console" class="link">Go to Pad</a>
		{:else if status === 'setup'}
			<SetupRequiredNotice
				{setupMethod}
				nextStep="An admin must finish setup before invitation links can be accepted."
				actionHref="/login"
				actionLabel="Go to login"
			/>
		{:else if status === 'not-joined'}
			<p class="subtitle error-text" role="alert">{errorMsg}</p>
			<a href="/console" class="link">Go to Pad</a>
		{:else if status === 'invalid'}
			<p class="subtitle error-text" role="alert" data-testid="join-invalid">
				This invitation link is invalid or has expired.
			</p>
			<p class="hint">Ask the person who invited you to send a new one.</p>
			<a href="/console" class="link">Go to Pad</a>
		{:else if status === 'error'}
			<p class="subtitle error-text" role="alert">{errorMsg}</p>
			<!-- Back to this invitation after signing in (TASK-2251): the code
			     used to be dropped here, stranding whoever followed the link. -->
			<a href={`/login?redirect=${encodeURIComponent(`/join/${code}`)}`} class="link">Go to login</a>
		{:else if status === '2fa'}
			<p class="subtitle">Two-factor authentication</p>

			<form class="form" method="post" novalidate onsubmit={(e) => { e.preventDefault(); handleVerify2FA(); }}>
				<p class="hint" id="join-totp-hint">Enter the 6-digit code from your authenticator app, or a recovery code.</p>

				<label class="field-label" for="join-totp">Authentication code</label>
				<input
					id="join-totp"
					name="totp"
					aria-describedby="join-totp-hint"
					use:autofocus
					type="text"
					placeholder="123456"
					bind:value={totpCode}
					disabled={submitting}
					autocomplete="one-time-code"
					inputmode="numeric"
				/>

				{#if formError}
					<p class="error" role="alert">{formError}</p>
				{/if}

				<button type="submit" disabled={submitting}>
					{#if submitting}
						Verifying...
					{:else}
						Verify
					{/if}
				</button>

				<button class="back-button" onclick={handleBack2FA} disabled={submitting} type="button">
					Back to sign in
				</button>
			</form>
		{:else}
			<p class="subtitle">
				You've been invited to
				{#if invitedWorkspaceName}<strong>{invitedWorkspaceName}</strong>{:else}a workspace{/if}
			</p>
			<p class="hint">{mode === 'register' ? 'Create an account' : 'Sign in'} to accept</p>

			<form class="form" method="post" novalidate onsubmit={(e) => { e.preventDefault(); handleSubmit(); }}>
				{#if mode === 'register'}
					<label class="field-label" for="join-name">Name</label>
					<input
						id="join-name"
						name="name"
						use:autofocus
						type="text"
						placeholder="Ada Lovelace"
						bind:value={name}
						oninput={handleNameInput}
						disabled={submitting}
						autocomplete="name"
					/>

					<div class="username-field">
						<label class="field-label" for="join-username">Username</label>
						<input
							id="join-username"
							name="username"
							aria-describedby="join-username-status"
							type="text"
							placeholder="ada"
							bind:value={username}
							oninput={handleUsernameInput}
							disabled={submitting}
							autocomplete="username"
						/>
						<span id="join-username-status" aria-live="polite">
							{#if usernameChecking}
								<span class="username-status checking">checking...</span>
							{:else if usernameAvailable === true}
								<span class="username-status available">available</span>
							{:else if usernameAvailable === false}
								<span class="username-status taken">{usernameError || 'not available'}</span>
							{/if}
						</span>
					</div>
				{/if}
				<label class="field-label" for="join-email">Email</label>
				<input
					id="join-email"
					name="email"
					aria-describedby={invitedEmail !== null ? 'join-email-hint' : undefined}
					use:autofocus={mode === 'login' && invitedEmail === null}
					type="email"
					placeholder="you@example.com"
					class:locked={invitedEmail !== null}
					bind:value={email}
					disabled={submitting}
					readonly={invitedEmail !== null}
					autocomplete="email"
				/>
				{#if invitedEmail !== null}
					<p class="field-hint" id="join-email-hint">This invitation was sent to this address.</p>
				{/if}
				<label class="field-label" for="join-password">Password</label>
				<input
					id="join-password"
					name="password"
					use:autofocus={mode === 'login' && invitedEmail !== null}
					type="password"
					bind:value={password}
					disabled={submitting}
					autocomplete={mode === 'register' ? 'new-password' : 'current-password'}
					aria-describedby={mode === 'register' ? passwordDescribedBy('join-password', passwordError) : undefined}
					aria-invalid={mode === 'register' && passwordError ? 'true' : undefined}
				/>
				{#if mode === 'register'}
					<PasswordRuleHint id="join-password" error={passwordError} />
					<label class="field-label" for="join-confirm-password">Confirm password</label>
					<input
						id="join-confirm-password"
						name="confirm-password"
						type="password"
						bind:value={confirmPassword}
						disabled={submitting}
						autocomplete="new-password"
					/>
				{/if}

				{#if formError}
					<p class="error" role="alert">{formError}</p>
				{/if}

				<button type="submit" disabled={submitting}>
					{#if submitting}
						{mode === 'register' ? 'Creating account...' : 'Signing in...'}
					{:else}
						{mode === 'register' ? 'Create account & join' : 'Sign in'}
					{/if}
				</button>
			</form>

			<AuthOAuthButtons
				cloudMode={authStore.cloudMode}
				redirectTarget={oauthRedirectTarget}
				{lastMethod}
				onProviderClick={handleOAuthClick}
			/>

			<p class="switch-mode">
				{#if mode === 'login'}
					Don't have an account? <button class="link-btn" onclick={() => { mode = 'register'; formError = ''; }}>Create one</button>
				{:else}
					Already have an account? <button class="link-btn" onclick={() => { mode = 'login'; formError = ''; }}>Sign in</button>
				{/if}
			</p>
		{/if}
	</div>

	<AuthFooter cloudMode={authStore.cloudMode} />
</div>

<style>
	.join-page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.join-page.cloud-mode {
		padding-top: 4rem;
	}

	.join-card {
		width: 100%;
		max-width: 380px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-10) var(--space-8);
		text-align: center;
	}

	.logo {
		font-size: 2rem;
		font-weight: 700;
		color: var(--text-primary);
		letter-spacing: -0.02em;
		margin-bottom: var(--space-2);
	}

	.subtitle {
		color: var(--text-secondary);
		font-size: 0.95rem;
		margin-bottom: var(--space-2);
	}

	.hint {
		color: var(--text-muted);
		font-size: 0.85rem;
		margin-bottom: var(--space-6);
		line-height: 1.4;
	}

	.error-text {
		color: var(--accent-red);
	}

	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		text-align: left;
	}

	input {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.95rem;
		font-family: var(--font-ui);
		outline: none;
		transition: border-color 0.15s;
	}
	input::placeholder { color: var(--text-muted); }
	input:focus { border-color: var(--accent-blue); }
	input:disabled { opacity: 0.6; }
	input.locked {
		color: var(--text-secondary);
		cursor: not-allowed;
	}
	input.locked:focus { border-color: var(--border); }

	.field-hint {
		color: var(--text-muted);
		font-size: 0.75rem;
		margin: calc(-1 * var(--space-2)) 0 0;
	}

	.error {
		color: var(--accent-red);
		font-size: 0.85rem;
	}

	button {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--accent-primary-strong); /* white text: AA in both themes (TASK-3509) */
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.95rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}
	button:hover:not(:disabled) { opacity: 0.9; }
	button:disabled { opacity: 0.6; cursor: not-allowed; }

	.secondary-button {
		background: transparent;
		color: var(--text-secondary);
		border: 1px solid var(--border);
	}
	.secondary-button:hover:not(:disabled) {
		color: var(--text-primary);
		opacity: 1;
	}

	.back-button {
		background: transparent;
		color: var(--text-muted);
		font-size: 0.85rem;
		font-weight: 400;
		padding: var(--space-2) var(--space-4);
	}
	.back-button:hover:not(:disabled) {
		color: var(--text-primary);
		opacity: 1;
	}

	.switch-mode {
		margin-top: var(--space-6);
		font-size: 0.85rem;
		color: var(--text-muted);
	}

	.link-btn {
		background: none;
		border: none;
		color: var(--accent-blue);
		cursor: pointer;
		font-size: inherit;
		padding: 0;
		width: auto;
		font-weight: 500;
	}
	.link-btn:hover { text-decoration: underline; }

	.link {
		color: var(--accent-blue);
		font-size: 0.9rem;
		text-decoration: none;
	}
	.link:hover { text-decoration: underline; }

	.username-field {
		position: relative;
	}

	.username-status {
		position: absolute;
		right: var(--space-3);
		top: 50%;
		transform: translateY(-50%);
		font-size: 0.75rem;
		pointer-events: none;
	}

	.username-status.checking {
		color: var(--text-muted);
	}

	.username-status.available {
		color: #22c55e;
	}

	.username-status.taken {
		color: var(--accent-red);
	}
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}
</style>
