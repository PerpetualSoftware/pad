// Setup for the `idb` vitest project (PLAN-2636 unit 1). Installs
// fake-indexeddb's in-memory IndexedDB as the global `indexedDB` (and the full
// set of `IDB*` interface globals — `IDBRequest`, `IDBKeyRange`, `IDBDatabase`
// … — which the `idb` wrapper references) so `localIndexPersistence` — which
// no-ops whenever `typeof indexedDB === 'undefined'` — actually exercises its
// write/read path under test. Before this project existed the entire
// persistence layer shipped on live-browser evidence runs only (Wren's #1148
// finding).
//
// `fake-indexeddb/auto` installs every IDB global once at import. A FRESH
// `IDBFactory` per test is the isolation boundary: every test starts with zero
// databases, so no cross-test state leaks through the shared in-memory store.
// The interface classes are stateless and stay put; only the factory (which
// owns the databases) is swapped. Tests that also need localIndexPersistence's
// own module-level connection cache reset pair this with `loadPersistence()`
// (vi.resetModules) from the idb harness.

import 'fake-indexeddb/auto';
import { IDBFactory } from 'fake-indexeddb';
import { beforeEach } from 'vitest';

beforeEach(() => {
	// A brand-new factory drops every database created by the previous test.
	// The IDB* interface globals installed by `/auto` above are unaffected.
	globalThis.indexedDB = new IDBFactory();
});
