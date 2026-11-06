import { doneFieldKey, doneFieldTerminalOptions, formatItemRef, itemUrlId, parseFields, parseSchema, type CalendarResponse, type Collection, type Item } from '$lib/types';

export type { CalendarConfig } from '$lib/collections/viewPersistence';
import type { CalendarConfig } from '$lib/collections/viewPersistence';
export const UNSUPPORTED_FIELD = "This field can't be used on a calendar. Choose another.";
export const STALE_FIELD = 'This date field is no longer available. Choose another field.';
export const PARTIAL_DAY = 'Too many items to show here. Filter the calendar, or use the list.';
const pad = (n: number) => String(n).padStart(2, '0');
const dateParts = (day: string) => day.split('-').map(Number);
const dateString = (date: Date) => `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`;

export function zoneDay(instant: Date, zone: string): string {
 const parts = new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(instant);
 const get = (key: string) => parts.find(p => p.type === key)!.value;
 return `${get('year')}-${get('month')}-${get('day')}`;
}

/** Find the first instant on a local date, including zones with a midnight gap.
 * UTC is only a search anchor; calendar dates are never advanced by 24 hours. */
export function dayBoundary(day: string, zone: string): string {
 const [y, m, d] = dateParts(day);
 const anchor = Date.UTC(y, m - 1, d) / 1000;
 const formatter = new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit' });
 const localDay = (seconds: number) => {
  const p = formatter.formatToParts(new Date(seconds * 1000));
  return ['year','month','day'].map(key => p.find(v => v.type === key)!.value).join('-');
 };
 let lo = anchor - 36 * 3600, hi = anchor + 36 * 3600;
 while (lo < hi) { const mid = Math.floor((lo + hi) / 2); if (localDay(mid) < day) lo = mid + 1; else hi = mid; }
 return new Date(lo * 1000).toISOString();
}
export function monthDays(month: string): string[] {
 const [y, m] = dateParts(month);
 return Array.from({length: new Date(Date.UTC(y, m, 0)).getUTCDate()}, (_, i) => `${month}-${pad(i + 1)}`);
}
export function shiftDay(day: string, amount: number): string {
 const [y, m, d] = dateParts(day);
 return dateString(new Date(Date.UTC(y, m - 1, d + amount)));
}
export function moveMonth(day: string, amount: number): string {
 const [y, m, d] = dateParts(day);
 const first = new Date(Date.UTC(y, m - 1 + amount, 1));
 const month = dateString(first).slice(0, 7);
 return `${month}-${pad(Math.min(d, monthDays(month).length))}`;
}
export function monthWindow(month: string, zone: string) {
 return { from: dayBoundary(`${month}-01`, zone), to: dayBoundary(moveMonth(`${month}-01`, 1), zone) };
}
export function dayLabel(day: string, full = false): string {
 return new Intl.DateTimeFormat('en-GB', { timeZone: 'UTC', ...(full ? { weekday: 'long' as const } : {}), day:'numeric',month:'long',year:'numeric' }).format(new Date(`${day}T12:00:00Z`));
}
export function monthLabel(month: string): string {
 return new Intl.DateTimeFormat('en-GB', {timeZone:'UTC',month:'long',year:'numeric'}).format(new Date(`${month}-01T12:00:00Z`));
}
export function timeLabel(at: string, zone: string): string {
 const date = new Date(at);
 return Number.isFinite(date.getTime()) ? new Intl.DateTimeFormat('en-GB', {timeZone:zone,hour:'2-digit',minute:'2-digit',hourCycle:'h23'}).format(date) : '';
}
/** Mirrors models.IsSafeFieldKey (internal/models/collection_traits.go): non-empty, only ASCII letters, digits, underscore, hyphen. */
export function isSafeFieldKey(key: string): boolean { return /^[A-Za-z0-9_-]+$/.test(key); }
/** Schema date fields the calendar API accepts as date_field or end_field: safe keys, and not shadowed by the built-in created/completed. */
export function dateFields(collection: Collection) { return parseSchema(collection).fields.filter(f => f.type === 'date' && isSafeFieldKey(f.key) && f.key !== 'created' && f.key !== 'completed'); }
export function fieldLabel(collection: Collection, key?: string): string {
 return key === 'created' ? 'Created' : key === 'completed' ? 'Completed' : dateFields(collection).find(f => f.key === key)?.label ?? key ?? 'Choose a date field';
}
export function staleConfig(collection: Collection, config: CalendarConfig): boolean {
 const fields = dateFields(collection);
 return !!((config.date_field && !['created','completed'].includes(config.date_field) && !fields.some(f => f.key === config.date_field)) || (config.end_field && !fields.some(f => f.key === config.end_field)));
}
export function calendarError(error: unknown): string {
 const e = error as {code?: string; details?: {field?: string; code?: string}};
 if (e?.code === 'validation_error' && ['unsupported_field_key','builtin_end_field'].includes(e.details?.code ?? '')) return UNSUPPORTED_FIELD;
 if (e?.code === 'validation_error' && ['date_field','end_field','config'].includes(e.details?.field ?? '')) return STALE_FIELD;
 if (e?.code === 'cap_window') return 'Choose a window of 92 days or less.';
 if (e?.code === 'invalid_tz' || e?.details?.field === 'tz') return 'This time zone is not available. Choose another in Settings.';
 return 'Could not load calendar.';
}
export interface CalendarRow {
 key: string; day: string; ref: string; target: string; title: string; allDay: boolean;
 item?: Item; at?: string; label?: string; terminal?: string; state?: string; overdue?: boolean;
 start?: string; end?: string; continues?: boolean; effectiveAt?: string; missedCount?: number; stale?: boolean;
}
/** Preserve server placement and order; expand only the supplied inclusive item span. */
export function calendarRows(data: CalendarResponse, collection: Collection, field: string, month?: string): CalendarRow[] {
 const rows: CalendarRow[] = [];
 const days = month ? monthDays(month) : undefined;
 for (const placed of data.items) {
  const item = placed.item, fields = parseFields(item);
  const status = fields[doneFieldKey(collection)];
  const terminal = typeof status === 'string' && doneFieldTerminalOptions(collection).includes(status) ? status : undefined;
  const at = field === 'created' ? item.created_at : field === 'completed' ? undefined : fields[field];
  const span = placed.end_day && placed.end_day > placed.day;
  const covered = span && days ? days.filter(d => d >= placed.day && d <= placed.end_day!) : [placed.day];
  for (const day of covered) {
   if (days && !days.includes(day)) continue;
   // A span cannot make the unexamined tail look complete.
   if (data.next_from && day >= data.next_from) continue;
   rows.push({key:`item:${item.id}:${day}`,day,item,ref:formatItemRef(item) ?? '',target:itemUrlId(item),title:item.title,terminal,
    allDay:!!span || (typeof at === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(at)),at:typeof at === 'string' ? at : undefined,
    label:fieldLabel(collection,field),start:span ? placed.day : undefined,end:span ? placed.end_day : undefined,continues:!!span && day !== placed.day});
  }
 }
 for (const [index, entry] of data.entries.entries()) {
  if (days && !days.includes(entry.day)) continue;
  rows.push({key:`schedule:${entry.schedule_id ?? index}:${entry.at}:${index}`,day:entry.day,ref:entry.item_ref,target:entry.item_ref,title:entry.item_title,
   allDay:!!entry.all_day,at:entry.at,state:entry.state,overdue:entry.overdue,effectiveAt:entry.effective_at,missedCount:entry.missed_count,stale:entry.stale});
 }
 return rows.sort((a,b) => a.day.localeCompare(b.day) || Number(b.allDay) - Number(a.allDay));
}
export function incomplete(data: CalendarResponse): boolean {
 return !!(data.truncated || data.partial_day || data.unexpanded?.length || data.unavailable?.length);
}
export function incompleteDay(data: CalendarResponse, day: string): boolean {
 return data.partial_day === day || !!(incomplete(data) && (!data.next_from || day >= data.next_from || data.unexpanded?.length || data.unavailable?.length));
}
export function calendarFilters(filters: Record<string,string>, tags: string[], search: string, unparented: boolean, options: {includeArchived?:boolean;structuredSearch?:boolean} = {}) {
 const params: Record<string,string> = {...filters};
 if (tags.length === 1) params.tag = tags[0];
 if (search.trim()) params.search = search.trim();
 if (unparented) params.unparented = 'true';
 const reserved = ['date_field','end_field','from','to','tz'].find(key => key in filters);
 const problem = options.includeArchived ? 'Archived items are not available in Calendar. Hide archived items, or use the list.'
  : options.structuredSearch ? 'The calendar does not support search shortcuts. Use plain text, or use the list.'
  : tags.length > 1 ? 'The calendar supports one tag filter at a time. Choose one tag, or use the list.'
  : reserved ? `The calendar cannot filter by “${reserved}”. Use the list for this filter.` : '';
 return { params, problem };
}
