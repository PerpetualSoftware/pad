// WebMCP / `document.modelContext` ambient type declarations (PLAN-1888 /
// TASK-1892).
//
// EXPERIMENTAL: `document.modelContext` is the browser-native WebMCP surface
// (a.k.a. the W3C Web Model Context proposal). No browser ships it as a
// stable API yet, and there are no `@types/*` packages for it. This stub
// exists only so the WebMCP module type-checks under `strict` — it is NOT a
// guarantee that the API is present at runtime. Every consumer MUST
// feature-detect (`'modelContext' in document`) before touching it.
//
// Shape follows the WebMCP spec's `registerTool(tool, opts)` form:
//   - tool: { name, description, inputSchema, annotations?, execute }
//   - opts: { signal?, exposedTo? }
// plus the `toolchange` event the host fires when the registered set
// changes. Kept intentionally narrow — only the members the module uses.

/** A JSON-Schema-ish input schema object for a WebMCP tool. */
interface ModelContextToolInputSchema {
	type: 'object';
	properties?: Record<string, unknown>;
	required?: string[];
	[key: string]: unknown;
}

/** Per-tool behavioural hints surfaced to the agent / consent UI. */
interface ModelContextToolAnnotations {
	/**
	 * True only when EVERY action the tool can perform is a pure read
	 * (DR-2). Mixed read/write tools omit this so the host prompts for
	 * per-invocation consent on every call.
	 */
	readOnlyHint?: boolean;
	/**
	 * Set when the tool's output can echo user-authored / untrusted
	 * content, so the agent treats the result as untrusted input.
	 */
	untrustedContentHint?: boolean;
	[key: string]: unknown;
}

/** One block of a tool-execution result. Only the text form is used here. */
interface ModelContextContentBlock {
	type: 'text';
	text: string;
}

/** The value a tool's `execute` resolves to. */
interface ModelContextToolResult {
	content: ModelContextContentBlock[];
	/** Optional flag the host inspects to surface the call as an error. */
	isError?: boolean;
}

/** A tool descriptor passed to `registerTool`. */
interface ModelContextTool {
	name: string;
	description: string;
	inputSchema: ModelContextToolInputSchema;
	annotations?: ModelContextToolAnnotations;
	execute: (args: Record<string, unknown>) => Promise<ModelContextToolResult>;
}

/** Options for `registerTool`. */
interface ModelContextRegisterToolOptions {
	/**
	 * Abort signal — when it fires, the host unregisters the tool. This is
	 * the canonical teardown path (workspace switch / unmount), so the
	 * module never has to track per-tool unregister handles.
	 */
	signal?: AbortSignal;
	/** Which agent surfaces the tool is exposed to (host-defined). */
	exposedTo?: string[];
}

/** The `document.modelContext` provider object. */
interface ModelContextProvider extends EventTarget {
	registerTool(tool: ModelContextTool, opts?: ModelContextRegisterToolOptions): void;
	addEventListener(type: 'toolchange', listener: (ev: Event) => void): void;
	removeEventListener(type: 'toolchange', listener: (ev: Event) => void): void;
}

interface Document {
	/**
	 * EXPERIMENTAL WebMCP provider. Optional — always feature-detect with
	 * `'modelContext' in document` before use.
	 */
	readonly modelContext?: ModelContextProvider;
}
