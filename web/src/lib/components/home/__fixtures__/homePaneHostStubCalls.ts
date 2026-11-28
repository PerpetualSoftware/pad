import type { HomePaneTarget } from '../homePane';

/** What the HomePaneHost stub was asked to do, for DesktopHome wiring tests. */
export const homePaneHostStubCalls = { open: [] as HomePaneTarget[], dismiss: [] as MouseEvent[] };
