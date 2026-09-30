// The prefix of the follows-title marker (BUG-3315), shared by the markdown
// load/save functions ($lib/utils/markdown) and the editor's link mark
// (components/editor/extensions/safeLink), which hides it from the DOM. It is a
// module of its own so that safeLink, which the headless materializer bundles,
// imports nothing DOM-dependent. See followsTitleMarker in $lib/utils/markdown.
export const FOLLOWS_TITLE_PREFIX = 'pad-follows-title:';
