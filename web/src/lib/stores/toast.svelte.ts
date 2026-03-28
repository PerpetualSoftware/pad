export interface Toast {
	id: string;
	message: string;
	type: 'success' | 'error' | 'info';
	duration: number;
}

export interface HistoryEntry {
	id: string;
	message: string;
	type: Toast['type'];
	timestamp: number;
}

const MAX_TOASTS = 5;
const MAX_HISTORY = 20;
const DEFAULT_DURATION = 3000;

let toasts = $state<Toast[]>([]);
let history = $state<HistoryEntry[]>([]);
let unreadCount = $state(0);
const timers = new Map<string, ReturnType<typeof setTimeout>>();

function generateId(): string {
	return Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
}

function show(message: string, type: Toast['type'] = 'info', duration: number = DEFAULT_DURATION): string {
	const id = generateId();
	const toast: Toast = { id, message, type, duration };

	toasts.push(toast);

	// Add to history
	history.unshift({ id, message, type, timestamp: Date.now() });
	while (history.length > MAX_HISTORY) {
		history.pop();
	}
	unreadCount++;

	// Drop oldest if exceeded max
	while (toasts.length > MAX_TOASTS) {
		const oldest = toasts.shift();
		if (oldest) {
			clearTimerFor(oldest.id);
		}
	}

	// Auto-dismiss after duration
	const timer = setTimeout(() => {
		dismiss(id);
	}, duration);
	timers.set(id, timer);

	return id;
}

function dismiss(id: string): void {
	clearTimerFor(id);
	const idx = toasts.findIndex((t) => t.id === id);
	if (idx !== -1) {
		toasts.splice(idx, 1);
	}
}

function clearTimerFor(id: string): void {
	const timer = timers.get(id);
	if (timer) {
		clearTimeout(timer);
		timers.delete(id);
	}
}

function markAllRead(): void {
	unreadCount = 0;
}

function clearHistory(): void {
	history.length = 0;
	unreadCount = 0;
}

export const toastStore = {
	get toasts(): Toast[] {
		return toasts;
	},
	get history(): HistoryEntry[] {
		return history;
	},
	get unreadCount(): number {
		return unreadCount;
	},
	show,
	dismiss,
	markAllRead,
	clearHistory
};
