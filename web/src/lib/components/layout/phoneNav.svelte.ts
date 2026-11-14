// Shared by the phone avatar and bar so only one docked menu is open.
export const phoneNav = $state<{ sheet: 'workspace' | 'capture' | 'account' | null }>({ sheet: null });
