// The editor's table options. Shared by the live editor (Editor.svelte, which
// adds its copy plugin on top) and (TASK-2198) the headless materializer
// bundle, so anything that changes the SCHEMA, renderHTML or markdown
// serialization belongs here, and nothing DOM- or component-dependent may be
// imported into this module.
export const PAD_TABLE_OPTIONS = { resizable: true, HTMLAttributes: { class: 'table-wrapper' } };
