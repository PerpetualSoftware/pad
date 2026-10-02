Pad is a project workspace: items such as tasks, bugs, ideas, plans and docs, organized in collections, each item with a short ref like TASK-12, fields like status and priority, markdown content, comments and links.

Start with list_workspaces, then get_workspace_overview for the workspace the user wants. Every other tool takes that workspace slug; pass it on each call.

To find something, use search or list_items, then get_item for one item's full content. Use list_collections before create_item to pick a collection and valid field values. Refer to items by their ref.

update_item changes only what you pass, and every content change is saved as a version first, so the user can undo it from the item's History. Explain a status change with add_comment. archive_item hides an item and restore_item brings it back: confirm with the user before archiving.

If list_workspaces returns no workspaces, tell the user to create one at https://app.getpad.dev and come back.
