/**
 * Tooltip for the marker on rows a workspace import wrote (BUG-3379). Import
 * keeps the export's author names and uploader ids verbatim, to restore
 * history; nothing here verified them, so the UI says so wherever it would
 * otherwise present them as authorship.
 */
export const IMPORTED_TITLE = 'Imported with the workspace — author not verified';
