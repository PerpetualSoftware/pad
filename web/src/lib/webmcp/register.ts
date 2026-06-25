// register.ts — the WebMCP lifecycle entry point (PLAN-1888 / TASK-1892,
// piece 5). Framework-light: a plain async function the workspace layout
// calls from its `$effect`. Returns a teardown function (or a no-op when the
// surface is unavailable). Registration is gated on:
//   1. webmcp_enabled (the session flag, DR-6)
//   2. feature detection — `document.modelContext` present (DR-6)
//   3. an authenticated user
// and unregistration rides an AbortSignal so workspace switch / unmount
// tears every tool down in one shot (no per-tool handles, no leaks).

import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { buildDescriptors } from './descriptors';
import { dispatch } from './dispatch';

/**
 * Feature-detect the WebMCP provider. Mirrors the
 * `'locks' in navigator` pattern in sse.svelte.ts:101 — narrow `in` check,
 * SSR-safe (`typeof document`).
 */
export function webmcpSupported(): boolean {
	return typeof document !== 'undefined' && 'modelContext' in document;
}

/** A teardown handle. Idempotent. */
export type WebMcpHandle = () => void;

const NOOP: WebMcpHandle = () => {};

/**
 * Register Pad's catalog tools with `document.modelContext`, scoped to
 * `wsSlug`. Returns a teardown function; calling it (or the layout aborting
 * the supplied controller) unregisters every tool.
 *
 * No-ops (returns NOOP) when the flag is off, the API is absent, or no user
 * is authenticated — with ZERO console noise, so a disabled instance has no
 * observable WebMCP behaviour (acceptance criterion).
 *
 * Re-entrancy: the caller (the layout `$effect`) is responsible for tearing
 * down the previous registration before calling again on workspace switch.
 * Each call owns its own AbortController so registrations never overlap.
 */
export async function registerWorkspaceTools(wsSlug: string): Promise<WebMcpHandle> {
	if (!wsSlug) return NOOP;

	// Gate 1: opt-in session flag (default/absent → false).
	if (!authStore.session?.webmcp_enabled) return NOOP;

	// Gate 2: native feature detection. Degrade silently.
	if (!webmcpSupported()) return NOOP;

	// Gate 3: authenticated user — the tools execute as the web session.
	if (!authStore.user) return NOOP;

	const modelContext = document.modelContext;
	if (!modelContext) return NOOP;

	// One controller per registration → AbortSignal-driven teardown.
	const controller = new AbortController();

	let surface;
	try {
		surface = await api.mcp.toolSurface();
	} catch {
		// Endpoint unreachable / unauthorized — degrade silently, nothing
		// registers. No console noise (acceptance criterion).
		return NOOP;
	}

	// The caller may have torn us down while the fetch was in flight (fast
	// workspace switch). Honour that and don't register stale tools.
	if (controller.signal.aborted) return NOOP;

	const descriptors = buildDescriptors(surface, (toolName) => {
		// Per-tool action read-only lookup, captured from this tool's actions.
		const tool = surface.tools.find((t) => t.name === toolName);
		const readOnly = new Map<string, boolean>(
			(tool?.actions ?? []).map((a) => [a.name, a.read_only]),
		);
		const isReadOnlyAction = (_name: string, action: string) =>
			readOnly.get(action);
		return (args: Record<string, unknown>) =>
			dispatch(api, wsSlug, isReadOnlyAction, toolName, args);
	});

	for (const { tool } of descriptors) {
		try {
			modelContext.registerTool(tool, { signal: controller.signal });
		} catch {
			// A host that rejects a single tool shouldn't abort the rest;
			// stay silent (acceptance: zero console noise).
		}
	}

	return () => controller.abort();
}
