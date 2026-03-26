import type { FieldDef, CollectionSettings } from '$lib/types';

export interface CollectionTemplate {
	id: string;
	name: string;
	icon: string;
	description: string;
	fields: FieldDef[];
	settings: CollectionSettings;
}

export const COLLECTION_TEMPLATES: CollectionTemplate[] = [
	{
		id: 'bug-tracker',
		name: 'Bug Tracker',
		icon: '🐛',
		description: 'Track bugs, defects, and issues',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['open', 'confirmed', 'in-progress', 'fixed', 'wont-fix'],
				default: 'open',
				required: true
			},
			{
				key: 'severity',
				label: 'Severity',
				type: 'select',
				options: ['low', 'medium', 'high', 'critical']
			},
			{
				key: 'priority',
				label: 'Priority',
				type: 'select',
				options: ['low', 'medium', 'high'],
				default: 'medium'
			},
			{ key: 'assignee', label: 'Assignee', type: 'text' },
			{ key: 'reported_by', label: 'Reported By', type: 'text' }
		],
		settings: {
			layout: 'fields-primary',
			default_view: 'board',
			board_group_by: 'status',
			list_sort_by: 'severity'
		}
	},
	{
		id: 'feature-requests',
		name: 'Feature Requests',
		icon: '⭐',
		description: 'Collect and prioritize feature requests',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['submitted', 'under-review', 'planned', 'building', 'shipped', 'declined'],
				default: 'submitted',
				required: true
			},
			{
				key: 'priority',
				label: 'Priority',
				type: 'select',
				options: ['low', 'medium', 'high']
			},
			{ key: 'votes', label: 'Votes', type: 'number' },
			{ key: 'category', label: 'Category', type: 'text' }
		],
		settings: {
			layout: 'balanced',
			default_view: 'board',
			board_group_by: 'status',
			list_sort_by: 'priority'
		}
	},
	{
		id: 'meeting-notes',
		name: 'Meeting Notes',
		icon: '📝',
		description: 'Capture meeting agendas, notes, and action items',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['draft', 'published'],
				default: 'draft',
				required: true
			},
			{
				key: 'type',
				label: 'Type',
				type: 'select',
				options: ['standup', 'planning', 'retro', '1-on-1', 'all-hands', 'other']
			},
			{ key: 'date', label: 'Date', type: 'date' },
			{ key: 'attendees', label: 'Attendees', type: 'text' }
		],
		settings: {
			layout: 'content-primary',
			default_view: 'list',
			list_sort_by: 'date',
			list_group_by: 'type'
		}
	},
	{
		id: 'decisions',
		name: 'Decisions',
		icon: '⚖️',
		description: 'Record architecture decisions and their rationale (ADRs)',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['proposed', 'accepted', 'rejected', 'superseded'],
				default: 'proposed',
				required: true
			},
			{ key: 'category', label: 'Category', type: 'text' },
			{ key: 'decision_date', label: 'Decision Date', type: 'date' }
		],
		settings: {
			layout: 'content-primary',
			default_view: 'list',
			list_sort_by: 'created_at',
			list_group_by: 'status'
		}
	},
	{
		id: 'tech-debt',
		name: 'Tech Debt',
		icon: '🔧',
		description: 'Track technical debt and improvement opportunities',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['identified', 'planned', 'in-progress', 'resolved'],
				default: 'identified',
				required: true
			},
			{
				key: 'severity',
				label: 'Severity',
				type: 'select',
				options: ['low', 'medium', 'high', 'critical']
			},
			{
				key: 'area',
				label: 'Area',
				type: 'select',
				options: ['backend', 'frontend', 'infrastructure', 'database', 'testing']
			}
		],
		settings: {
			layout: 'balanced',
			default_view: 'board',
			board_group_by: 'status',
			list_sort_by: 'severity'
		}
	},
	{
		id: 'okrs',
		name: 'OKRs',
		icon: '🎯',
		description: 'Set objectives and track key results',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['draft', 'active', 'completed', 'abandoned'],
				default: 'draft',
				required: true
			},
			{ key: 'quarter', label: 'Quarter', type: 'text' },
			{
				key: 'type',
				label: 'Type',
				type: 'select',
				options: ['objective', 'key-result']
			},
			{ key: 'progress', label: 'Progress', type: 'number', suffix: '%' }
		],
		settings: {
			layout: 'balanced',
			default_view: 'list',
			list_sort_by: 'quarter',
			list_group_by: 'type'
		}
	},
	{
		id: 'sprint-backlog',
		name: 'Sprint Backlog',
		icon: '🏃',
		description: 'Manage sprint work items and track progress',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['backlog', 'ready', 'in-progress', 'review', 'done'],
				default: 'backlog',
				required: true
			},
			{
				key: 'priority',
				label: 'Priority',
				type: 'select',
				options: ['low', 'medium', 'high'],
				default: 'medium'
			},
			{ key: 'story_points', label: 'Story Points', type: 'number' },
			{ key: 'assignee', label: 'Assignee', type: 'text' },
			{ key: 'sprint', label: 'Sprint', type: 'text' }
		],
		settings: {
			layout: 'fields-primary',
			default_view: 'board',
			board_group_by: 'status',
			list_sort_by: 'priority'
		}
	},
	{
		id: 'knowledge-base',
		name: 'Knowledge Base',
		icon: '📚',
		description: 'Organize documentation, guides, and reference material',
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['draft', 'published', 'archived'],
				default: 'draft',
				required: true
			},
			{ key: 'category', label: 'Category', type: 'text' },
			{
				key: 'audience',
				label: 'Audience',
				type: 'select',
				options: ['team', 'public', 'onboarding']
			}
		],
		settings: {
			layout: 'content-primary',
			default_view: 'list',
			list_sort_by: 'updated_at',
			list_group_by: 'category'
		}
	}
];
