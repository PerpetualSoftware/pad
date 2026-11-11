import type { AgendaResponse } from '../src/lib/types';

// The marker distinguishes an unlisted workspace group from an empty personal list.
type Assert<T extends true> = T;
type Scope = AgendaResponse['no_date_scope'];
export type AgendaNoDateScopeContract = Assert<
 Scope extends 'personal_only' | undefined
  ? ('personal_only' | undefined extends Scope ? true : false)
  : false
>;
