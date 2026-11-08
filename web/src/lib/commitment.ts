import type { CommitmentRoles } from '$lib/types';

export function commitmentKind(roles: Pick<CommitmentRoles, 'deadline' | 'occupied_until'>): 'deadline_hard' | 'window' | 'check' {
 if (roles.deadline !== null) return 'deadline_hard';
 if (roles.occupied_until !== null) return 'window';
 return 'check';
}
