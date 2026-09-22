// The copy dialog's preview debounce (CopyItemDialog.svelte), in a plain module
// so the e2e spec that measures the runner can derive its timings from it
// (BUG-3151): a change here retunes the spec instead of silently weakening it.
export const COPY_PREFLIGHT_DEBOUNCE_MS = 250;
