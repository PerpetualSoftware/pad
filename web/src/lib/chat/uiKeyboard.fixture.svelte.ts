// A reactive stand-in for uiStore.keyboardVisible and uiStore.isMobile, for tests that
// cannot raise a real keyboard or resize a real viewport.
export const kb = $state({ visible: false, mobile: false });
