// A reactive stand-in for uiStore.keyboardVisible, for tests that cannot raise a real keyboard.
export const kb = $state({ visible: false });
