-- Layer B of the NUL invariant (DOC-2823 S2): the database owns the rule.
--
-- GENERATED from internal/store/nulcolumns.go. Do not edit by hand — run
--   GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestGenerateNULTriggerMigration
-- and commit the result. TestNULTriggersMatchTheList fails if this file and the
-- Go list disagree.
--
-- WHY TRIGGERS AND NOT ONLY THE GO GUARD. S1's Layer A makes the invariant a
-- property of the BINARY. BUG-2813 is the window where that is not enough: an
-- older binary serving the same SQLite file has no such guard, and a rollback,
-- a staged rollout or a second instance writes rows the invariant forbids.
-- A trigger is enforced by the FILE, so it holds for every writer.
--
-- SQLITE ONLY. Postgres refuses a NUL in text natively (SQLSTATE 22021) and an
-- escape decoding to one in jsonb (22P05), so the database already owns the
-- rule there; adding triggers would duplicate an existing guarantee.
--
-- PREDICATE, measured on modernc.org/sqlite v1.57.0 / SQLite 3.53.3 in
-- TASK-2824 rather than assumed:
--   * instr(col, char(0)) finds a real NUL. length() does NOT — it C-truncates
--     at the NUL — so instr is the only builtin to trust here.
--   * json_tree DECODES the six-character escape, exposing a real NUL in both
--     the value and key columns, and a DOUBLED backslash stays literal text
--     with no false positive.
--   * a Go-bound real-NUL parameter reaches the trigger intact, which is what
--     makes BEFORE INSERT the right shape: an old binary's write is seen.
--
-- No DB-side repair is attempted, deliberately: the same measurement shows
-- SQLite's string functions disagree about NUL-bearing text, so transforming
-- such a value in SQL cannot be trusted. Repair is S3, in Go.

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_actor_ins
BEFORE INSERT ON activities
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_actor_upd
BEFORE UPDATE OF actor ON activities
FOR EACH ROW WHEN NEW.actor IS NOT NULL AND (
			instr(NEW.actor, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.actor must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_ip_address_ins
BEFORE INSERT ON activities
FOR EACH ROW WHEN NEW.ip_address IS NOT NULL AND (
			instr(NEW.ip_address, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.ip_address must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_ip_address_upd
BEFORE UPDATE OF ip_address ON activities
FOR EACH ROW WHEN NEW.ip_address IS NOT NULL AND (
			instr(NEW.ip_address, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.ip_address must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_metadata_ins
BEFORE INSERT ON activities
FOR EACH ROW WHEN NEW.metadata IS NOT NULL AND (
			instr(NEW.metadata, char(0)) > 0
			OR (json_valid(NEW.metadata) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.metadata)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.metadata must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_metadata_upd
BEFORE UPDATE OF metadata ON activities
FOR EACH ROW WHEN NEW.metadata IS NOT NULL AND (
			instr(NEW.metadata, char(0)) > 0
			OR (json_valid(NEW.metadata) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.metadata)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.metadata must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_user_agent_ins
BEFORE INSERT ON activities
FOR EACH ROW WHEN NEW.user_agent IS NOT NULL AND (
			instr(NEW.user_agent, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.user_agent must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_activities_user_agent_upd
BEFORE UPDATE OF user_agent ON activities
FOR EACH ROW WHEN NEW.user_agent IS NOT NULL AND (
			instr(NEW.user_agent, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: activities.user_agent must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_description_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_description_upd
BEFORE UPDATE OF description ON agent_roles
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_icon_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.icon IS NOT NULL AND (
			instr(NEW.icon, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.icon must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_icon_upd
BEFORE UPDATE OF icon ON agent_roles
FOR EACH ROW WHEN NEW.icon IS NOT NULL AND (
			instr(NEW.icon, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.icon must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_name_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_name_upd
BEFORE UPDATE OF name ON agent_roles
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_slug_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_slug_upd
BEFORE UPDATE OF slug ON agent_roles
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_tools_ins
BEFORE INSERT ON agent_roles
FOR EACH ROW WHEN NEW.tools IS NOT NULL AND (
			instr(NEW.tools, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.tools must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_agent_roles_tools_upd
BEFORE UPDATE OF tools ON agent_roles
FOR EACH ROW WHEN NEW.tools IS NOT NULL AND (
			instr(NEW.tools, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: agent_roles.tools must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_api_tokens_name_ins
BEFORE INSERT ON api_tokens
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: api_tokens.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_api_tokens_name_upd
BEFORE UPDATE OF name ON api_tokens
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: api_tokens.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_api_tokens_scopes_ins
BEFORE INSERT ON api_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
			OR (json_valid(NEW.scopes) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.scopes)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: api_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_api_tokens_scopes_upd
BEFORE UPDATE OF scopes ON api_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
			OR (json_valid(NEW.scopes) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.scopes)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: api_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_attachments_filename_ins
BEFORE INSERT ON attachments
FOR EACH ROW WHEN NEW.filename IS NOT NULL AND (
			instr(NEW.filename, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: attachments.filename must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_attachments_filename_upd
BEFORE UPDATE OF filename ON attachments
FOR EACH ROW WHEN NEW.filename IS NOT NULL AND (
			instr(NEW.filename, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: attachments.filename must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_attachments_mime_type_ins
BEFORE INSERT ON attachments
FOR EACH ROW WHEN NEW.mime_type IS NOT NULL AND (
			instr(NEW.mime_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: attachments.mime_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_attachments_mime_type_upd
BEFORE UPDATE OF mime_type ON attachments
FOR EACH ROW WHEN NEW.mime_type IS NOT NULL AND (
			instr(NEW.mime_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: attachments.mime_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_description_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_description_upd
BEFORE UPDATE OF description ON collections
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_icon_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.icon IS NOT NULL AND (
			instr(NEW.icon, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.icon must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_icon_upd
BEFORE UPDATE OF icon ON collections
FOR EACH ROW WHEN NEW.icon IS NOT NULL AND (
			instr(NEW.icon, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.icon must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_name_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_name_upd
BEFORE UPDATE OF name ON collections
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_prefix_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.prefix IS NOT NULL AND (
			instr(NEW.prefix, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.prefix must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_prefix_upd
BEFORE UPDATE OF prefix ON collections
FOR EACH ROW WHEN NEW.prefix IS NOT NULL AND (
			instr(NEW.prefix, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.prefix must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_schema_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.schema IS NOT NULL AND (
			instr(NEW.schema, char(0)) > 0
			OR (json_valid(NEW.schema) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.schema)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.schema must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_schema_upd
BEFORE UPDATE OF schema ON collections
FOR EACH ROW WHEN NEW.schema IS NOT NULL AND (
			instr(NEW.schema, char(0)) > 0
			OR (json_valid(NEW.schema) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.schema)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.schema must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_settings_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.settings IS NOT NULL AND (
			instr(NEW.settings, char(0)) > 0
			OR (json_valid(NEW.settings) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.settings)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.settings must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_settings_upd
BEFORE UPDATE OF settings ON collections
FOR EACH ROW WHEN NEW.settings IS NOT NULL AND (
			instr(NEW.settings, char(0)) > 0
			OR (json_valid(NEW.settings) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.settings)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.settings must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_slug_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_slug_upd
BEFORE UPDATE OF slug ON collections
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_traits_ins
BEFORE INSERT ON collections
FOR EACH ROW WHEN NEW.traits IS NOT NULL AND (
			instr(NEW.traits, char(0)) > 0
			OR (json_valid(NEW.traits) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.traits)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.traits must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_collections_traits_upd
BEFORE UPDATE OF traits ON collections
FOR EACH ROW WHEN NEW.traits IS NOT NULL AND (
			instr(NEW.traits, char(0)) > 0
			OR (json_valid(NEW.traits) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.traits)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: collections.traits must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comment_reactions_emoji_ins
BEFORE INSERT ON comment_reactions
FOR EACH ROW WHEN NEW.emoji IS NOT NULL AND (
			instr(NEW.emoji, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comment_reactions.emoji must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comment_reactions_emoji_upd
BEFORE UPDATE OF emoji ON comment_reactions
FOR EACH ROW WHEN NEW.emoji IS NOT NULL AND (
			instr(NEW.emoji, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comment_reactions.emoji must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_author_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.author IS NOT NULL AND (
			instr(NEW.author, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.author must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_author_upd
BEFORE UPDATE OF author ON comments
FOR EACH ROW WHEN NEW.author IS NOT NULL AND (
			instr(NEW.author, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.author must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_body_ins
BEFORE INSERT ON comments
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_comments_body_upd
BEFORE UPDATE OF body ON comments
FOR EACH ROW WHEN NEW.body IS NOT NULL AND (
			instr(NEW.body, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: comments.body must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_content_ins
BEFORE INSERT ON custom_templates
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_content_upd
BEFORE UPDATE OF content ON custom_templates
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_description_ins
BEFORE INSERT ON custom_templates
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_description_upd
BEFORE UPDATE OF description ON custom_templates
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_doc_type_ins
BEFORE INSERT ON custom_templates
FOR EACH ROW WHEN NEW.doc_type IS NOT NULL AND (
			instr(NEW.doc_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.doc_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_doc_type_upd
BEFORE UPDATE OF doc_type ON custom_templates
FOR EACH ROW WHEN NEW.doc_type IS NOT NULL AND (
			instr(NEW.doc_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.doc_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_name_ins
BEFORE INSERT ON custom_templates
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_custom_templates_name_upd
BEFORE UPDATE OF name ON custom_templates
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: custom_templates.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_content_ins
BEFORE INSERT ON documents
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_content_upd
BEFORE UPDATE OF content ON documents
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_doc_type_ins
BEFORE INSERT ON documents
FOR EACH ROW WHEN NEW.doc_type IS NOT NULL AND (
			instr(NEW.doc_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.doc_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_doc_type_upd
BEFORE UPDATE OF doc_type ON documents
FOR EACH ROW WHEN NEW.doc_type IS NOT NULL AND (
			instr(NEW.doc_type, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.doc_type must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_tags_ins
BEFORE INSERT ON documents
FOR EACH ROW WHEN NEW.tags IS NOT NULL AND (
			instr(NEW.tags, char(0)) > 0
			OR (json_valid(NEW.tags) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.tags)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.tags must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_tags_upd
BEFORE UPDATE OF tags ON documents
FOR EACH ROW WHEN NEW.tags IS NOT NULL AND (
			instr(NEW.tags, char(0)) > 0
			OR (json_valid(NEW.tags) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.tags)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.tags must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_title_ins
BEFORE INSERT ON documents
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_documents_title_upd
BEFORE UPDATE OF title ON documents
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: documents.title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_email_optouts_email_ins
BEFORE INSERT ON email_optouts
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: email_optouts.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_email_optouts_email_upd
BEFORE UPDATE OF email ON email_optouts
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: email_optouts.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_event_outbox_payload_ins
BEFORE INSERT ON event_outbox
FOR EACH ROW WHEN NEW.payload IS NOT NULL AND (
			instr(NEW.payload, char(0)) > 0
			OR (json_valid(NEW.payload) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.payload)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: event_outbox.payload must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_event_outbox_payload_upd
BEFORE UPDATE OF payload ON event_outbox
FOR EACH ROW WHEN NEW.payload IS NOT NULL AND (
			instr(NEW.payload, char(0)) > 0
			OR (json_valid(NEW.payload) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.payload)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: event_outbox.payload must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_versions_change_summary_ins
BEFORE INSERT ON item_versions
FOR EACH ROW WHEN NEW.change_summary IS NOT NULL AND (
			instr(NEW.change_summary, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_versions.change_summary must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_versions_change_summary_upd
BEFORE UPDATE OF change_summary ON item_versions
FOR EACH ROW WHEN NEW.change_summary IS NOT NULL AND (
			instr(NEW.change_summary, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_versions.change_summary must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_versions_content_ins
BEFORE INSERT ON item_versions
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_versions.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_versions_content_upd
BEFORE UPDATE OF content ON item_versions
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_versions.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_display_text_ins
BEFORE INSERT ON item_wiki_links
FOR EACH ROW WHEN NEW.display_text IS NOT NULL AND (
			instr(NEW.display_text, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.display_text must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_display_text_upd
BEFORE UPDATE OF display_text ON item_wiki_links
FOR EACH ROW WHEN NEW.display_text IS NOT NULL AND (
			instr(NEW.display_text, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.display_text must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_target_ref_ins
BEFORE INSERT ON item_wiki_links
FOR EACH ROW WHEN NEW.target_ref IS NOT NULL AND (
			instr(NEW.target_ref, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.target_ref must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_target_ref_upd
BEFORE UPDATE OF target_ref ON item_wiki_links
FOR EACH ROW WHEN NEW.target_ref IS NOT NULL AND (
			instr(NEW.target_ref, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.target_ref must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_target_title_ins
BEFORE INSERT ON item_wiki_links
FOR EACH ROW WHEN NEW.target_title IS NOT NULL AND (
			instr(NEW.target_title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.target_title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_item_wiki_links_target_title_upd
BEFORE UPDATE OF target_title ON item_wiki_links
FOR EACH ROW WHEN NEW.target_title IS NOT NULL AND (
			instr(NEW.target_title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: item_wiki_links.target_title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_content_ins
BEFORE INSERT ON items
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_content_upd
BEFORE UPDATE OF content ON items
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_fields_ins
BEFORE INSERT ON items
FOR EACH ROW WHEN NEW.fields IS NOT NULL AND (
			instr(NEW.fields, char(0)) > 0
			OR (json_valid(NEW.fields) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.fields)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.fields must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_fields_upd
BEFORE UPDATE OF fields ON items
FOR EACH ROW WHEN NEW.fields IS NOT NULL AND (
			instr(NEW.fields, char(0)) > 0
			OR (json_valid(NEW.fields) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.fields)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.fields must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_tags_ins
BEFORE INSERT ON items
FOR EACH ROW WHEN NEW.tags IS NOT NULL AND (
			instr(NEW.tags, char(0)) > 0
			OR (json_valid(NEW.tags) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.tags)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.tags must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_tags_upd
BEFORE UPDATE OF tags ON items
FOR EACH ROW WHEN NEW.tags IS NOT NULL AND (
			instr(NEW.tags, char(0)) > 0
			OR (json_valid(NEW.tags) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.tags)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.tags must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_title_ins
BEFORE INSERT ON items
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_items_title_upd
BEFORE UPDATE OF title ON items
FOR EACH ROW WHEN NEW.title IS NOT NULL AND (
			instr(NEW.title, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: items.title must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_mcp_audit_log_request_id_ins
BEFORE INSERT ON mcp_audit_log
FOR EACH ROW WHEN NEW.request_id IS NOT NULL AND (
			instr(NEW.request_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: mcp_audit_log.request_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_mcp_audit_log_request_id_upd
BEFORE UPDATE OF request_id ON mcp_audit_log
FOR EACH ROW WHEN NEW.request_id IS NOT NULL AND (
			instr(NEW.request_id, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: mcp_audit_log.request_id must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_audience_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_audience_upd
BEFORE UPDATE OF audience ON oauth_access_tokens
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_granted_audience_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_granted_audience_upd
BEFORE UPDATE OF granted_audience ON oauth_access_tokens
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_granted_scopes_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_granted_scopes_upd
BEFORE UPDATE OF granted_scopes ON oauth_access_tokens
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_request_form_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_request_form_upd
BEFORE UPDATE OF request_form ON oauth_access_tokens
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_scopes_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_scopes_upd
BEFORE UPDATE OF scopes ON oauth_access_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_session_data_ins
BEFORE INSERT ON oauth_access_tokens
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_access_tokens_session_data_upd
BEFORE UPDATE OF session_data ON oauth_access_tokens
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_access_tokens.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_audience_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_audience_upd
BEFORE UPDATE OF audience ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_granted_audience_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_granted_audience_upd
BEFORE UPDATE OF granted_audience ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_granted_scopes_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_granted_scopes_upd
BEFORE UPDATE OF granted_scopes ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_request_form_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_request_form_upd
BEFORE UPDATE OF request_form ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_scopes_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_scopes_upd
BEFORE UPDATE OF scopes ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_session_data_ins
BEFORE INSERT ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_authorization_codes_session_data_upd
BEFORE UPDATE OF session_data ON oauth_authorization_codes
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_authorization_codes.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_grant_types_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.grant_types IS NOT NULL AND (
			instr(NEW.grant_types, char(0)) > 0
			OR (json_valid(NEW.grant_types) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.grant_types)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.grant_types must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_grant_types_upd
BEFORE UPDATE OF grant_types ON oauth_clients
FOR EACH ROW WHEN NEW.grant_types IS NOT NULL AND (
			instr(NEW.grant_types, char(0)) > 0
			OR (json_valid(NEW.grant_types) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.grant_types)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.grant_types must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_logo_url_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.logo_url IS NOT NULL AND (
			instr(NEW.logo_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.logo_url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_logo_url_upd
BEFORE UPDATE OF logo_url ON oauth_clients
FOR EACH ROW WHEN NEW.logo_url IS NOT NULL AND (
			instr(NEW.logo_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.logo_url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_name_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_name_upd
BEFORE UPDATE OF name ON oauth_clients
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_redirect_uris_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.redirect_uris IS NOT NULL AND (
			instr(NEW.redirect_uris, char(0)) > 0
			OR (json_valid(NEW.redirect_uris) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.redirect_uris)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.redirect_uris must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_redirect_uris_upd
BEFORE UPDATE OF redirect_uris ON oauth_clients
FOR EACH ROW WHEN NEW.redirect_uris IS NOT NULL AND (
			instr(NEW.redirect_uris, char(0)) > 0
			OR (json_valid(NEW.redirect_uris) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.redirect_uris)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.redirect_uris must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_response_types_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.response_types IS NOT NULL AND (
			instr(NEW.response_types, char(0)) > 0
			OR (json_valid(NEW.response_types) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.response_types)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.response_types must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_response_types_upd
BEFORE UPDATE OF response_types ON oauth_clients
FOR EACH ROW WHEN NEW.response_types IS NOT NULL AND (
			instr(NEW.response_types, char(0)) > 0
			OR (json_valid(NEW.response_types) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.response_types)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.response_types must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_scopes_ins
BEFORE INSERT ON oauth_clients
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
			OR (json_valid(NEW.scopes) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.scopes)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_clients_scopes_upd
BEFORE UPDATE OF scopes ON oauth_clients
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
			OR (json_valid(NEW.scopes) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.scopes)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_clients.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_connections_name_ins
BEFORE INSERT ON oauth_connections
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_connections.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_connections_name_upd
BEFORE UPDATE OF name ON oauth_connections
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_connections.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_audience_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_audience_upd
BEFORE UPDATE OF audience ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_granted_audience_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_granted_audience_upd
BEFORE UPDATE OF granted_audience ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_granted_scopes_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_granted_scopes_upd
BEFORE UPDATE OF granted_scopes ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_request_form_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_request_form_upd
BEFORE UPDATE OF request_form ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_scopes_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_scopes_upd
BEFORE UPDATE OF scopes ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_session_data_ins
BEFORE INSERT ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_pkce_requests_session_data_upd
BEFORE UPDATE OF session_data ON oauth_pkce_requests
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_pkce_requests.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_audience_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_audience_upd
BEFORE UPDATE OF audience ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.audience IS NOT NULL AND (
			instr(NEW.audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_granted_audience_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_granted_audience_upd
BEFORE UPDATE OF granted_audience ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.granted_audience IS NOT NULL AND (
			instr(NEW.granted_audience, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.granted_audience must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_granted_scopes_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_granted_scopes_upd
BEFORE UPDATE OF granted_scopes ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.granted_scopes IS NOT NULL AND (
			instr(NEW.granted_scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.granted_scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_request_form_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_request_form_upd
BEFORE UPDATE OF request_form ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.request_form IS NOT NULL AND (
			instr(NEW.request_form, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.request_form must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_scopes_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_scopes_upd
BEFORE UPDATE OF scopes ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.scopes IS NOT NULL AND (
			instr(NEW.scopes, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.scopes must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_session_data_ins
BEFORE INSERT ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_oauth_refresh_tokens_session_data_upd
BEFORE UPDATE OF session_data ON oauth_refresh_tokens
FOR EACH ROW WHEN NEW.session_data IS NOT NULL AND (
			instr(NEW.session_data, char(0)) > 0
			OR (json_valid(NEW.session_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.session_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: oauth_refresh_tokens.session_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_platform_settings_value_ins
BEFORE INSERT ON platform_settings
FOR EACH ROW WHEN NEW.value IS NOT NULL AND (
			instr(NEW.value, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: platform_settings.value must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_platform_settings_value_upd
BEFORE UPDATE OF value ON platform_settings
FOR EACH ROW WHEN NEW.value IS NOT NULL AND (
			instr(NEW.value, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: platform_settings.value must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_progress_snapshots_phase_data_ins
BEFORE INSERT ON progress_snapshots
FOR EACH ROW WHEN NEW.phase_data IS NOT NULL AND (
			instr(NEW.phase_data, char(0)) > 0
			OR (json_valid(NEW.phase_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.phase_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: progress_snapshots.phase_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_progress_snapshots_phase_data_upd
BEFORE UPDATE OF phase_data ON progress_snapshots
FOR EACH ROW WHEN NEW.phase_data IS NOT NULL AND (
			instr(NEW.phase_data, char(0)) > 0
			OR (json_valid(NEW.phase_data) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.phase_data)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: progress_snapshots.phase_data must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_sessions_device_info_ins
BEFORE INSERT ON sessions
FOR EACH ROW WHEN NEW.device_info IS NOT NULL AND (
			instr(NEW.device_info, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: sessions.device_info must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_sessions_device_info_upd
BEFORE UPDATE OF device_info ON sessions
FOR EACH ROW WHEN NEW.device_info IS NOT NULL AND (
			instr(NEW.device_info, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: sessions.device_info must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_sessions_ip_address_ins
BEFORE INSERT ON sessions
FOR EACH ROW WHEN NEW.ip_address IS NOT NULL AND (
			instr(NEW.ip_address, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: sessions.ip_address must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_sessions_ip_address_upd
BEFORE UPDATE OF ip_address ON sessions
FOR EACH ROW WHEN NEW.ip_address IS NOT NULL AND (
			instr(NEW.ip_address, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: sessions.ip_address must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_share_links_restrict_to_email_ins
BEFORE INSERT ON share_links
FOR EACH ROW WHEN NEW.restrict_to_email IS NOT NULL AND (
			instr(NEW.restrict_to_email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: share_links.restrict_to_email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_share_links_restrict_to_email_upd
BEFORE UPDATE OF restrict_to_email ON share_links
FOR EACH ROW WHEN NEW.restrict_to_email IS NOT NULL AND (
			instr(NEW.restrict_to_email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: share_links.restrict_to_email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_status_transitions_from_status_ins
BEFORE INSERT ON status_transitions
FOR EACH ROW WHEN NEW.from_status IS NOT NULL AND (
			instr(NEW.from_status, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: status_transitions.from_status must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_status_transitions_from_status_upd
BEFORE UPDATE OF from_status ON status_transitions
FOR EACH ROW WHEN NEW.from_status IS NOT NULL AND (
			instr(NEW.from_status, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: status_transitions.from_status must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_status_transitions_to_status_ins
BEFORE INSERT ON status_transitions
FOR EACH ROW WHEN NEW.to_status IS NOT NULL AND (
			instr(NEW.to_status, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: status_transitions.to_status must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_status_transitions_to_status_upd
BEFORE UPDATE OF to_status ON status_transitions
FOR EACH ROW WHEN NEW.to_status IS NOT NULL AND (
			instr(NEW.to_status, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: status_transitions.to_status must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_user_report_layouts_config_ins
BEFORE INSERT ON user_report_layouts
FOR EACH ROW WHEN NEW.config IS NOT NULL AND (
			instr(NEW.config, char(0)) > 0
			OR (json_valid(NEW.config) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.config)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: user_report_layouts.config must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_user_report_layouts_config_upd
BEFORE UPDATE OF config ON user_report_layouts
FOR EACH ROW WHEN NEW.config IS NOT NULL AND (
			instr(NEW.config, char(0)) > 0
			OR (json_valid(NEW.config) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.config)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: user_report_layouts.config must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_avatar_url_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.avatar_url IS NOT NULL AND (
			instr(NEW.avatar_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.avatar_url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_avatar_url_upd
BEFORE UPDATE OF avatar_url ON users
FOR EACH ROW WHEN NEW.avatar_url IS NOT NULL AND (
			instr(NEW.avatar_url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.avatar_url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_email_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_email_upd
BEFORE UPDATE OF email ON users
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_name_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_name_upd
BEFORE UPDATE OF name ON users
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_oauth_providers_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.oauth_providers IS NOT NULL AND (
			instr(NEW.oauth_providers, char(0)) > 0
			OR (json_valid(NEW.oauth_providers) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.oauth_providers)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.oauth_providers must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_oauth_providers_upd
BEFORE UPDATE OF oauth_providers ON users
FOR EACH ROW WHEN NEW.oauth_providers IS NOT NULL AND (
			instr(NEW.oauth_providers, char(0)) > 0
			OR (json_valid(NEW.oauth_providers) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.oauth_providers)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.oauth_providers must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_plan_overrides_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.plan_overrides IS NOT NULL AND (
			instr(NEW.plan_overrides, char(0)) > 0
			OR (json_valid(NEW.plan_overrides) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.plan_overrides)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.plan_overrides must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_plan_overrides_upd
BEFORE UPDATE OF plan_overrides ON users
FOR EACH ROW WHEN NEW.plan_overrides IS NOT NULL AND (
			instr(NEW.plan_overrides, char(0)) > 0
			OR (json_valid(NEW.plan_overrides) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.plan_overrides)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.plan_overrides must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_username_ins
BEFORE INSERT ON users
FOR EACH ROW WHEN NEW.username IS NOT NULL AND (
			instr(NEW.username, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.username must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_users_username_upd
BEFORE UPDATE OF username ON users
FOR EACH ROW WHEN NEW.username IS NOT NULL AND (
			instr(NEW.username, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: users.username must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_versions_change_summary_ins
BEFORE INSERT ON versions
FOR EACH ROW WHEN NEW.change_summary IS NOT NULL AND (
			instr(NEW.change_summary, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: versions.change_summary must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_versions_change_summary_upd
BEFORE UPDATE OF change_summary ON versions
FOR EACH ROW WHEN NEW.change_summary IS NOT NULL AND (
			instr(NEW.change_summary, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: versions.change_summary must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_versions_content_ins
BEFORE INSERT ON versions
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: versions.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_versions_content_upd
BEFORE UPDATE OF content ON versions
FOR EACH ROW WHEN NEW.content IS NOT NULL AND (
			instr(NEW.content, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: versions.content must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_config_ins
BEFORE INSERT ON views
FOR EACH ROW WHEN NEW.config IS NOT NULL AND (
			instr(NEW.config, char(0)) > 0
			OR (json_valid(NEW.config) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.config)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.config must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_config_upd
BEFORE UPDATE OF config ON views
FOR EACH ROW WHEN NEW.config IS NOT NULL AND (
			instr(NEW.config, char(0)) > 0
			OR (json_valid(NEW.config) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.config)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.config must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_name_ins
BEFORE INSERT ON views
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_name_upd
BEFORE UPDATE OF name ON views
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_slug_ins
BEFORE INSERT ON views
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_views_slug_upd
BEFORE UPDATE OF slug ON views
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: views.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_watches_predicate_ins
BEFORE INSERT ON watches
FOR EACH ROW WHEN NEW.predicate IS NOT NULL AND (
			instr(NEW.predicate, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: watches.predicate must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_watches_predicate_upd
BEFORE UPDATE OF predicate ON watches
FOR EACH ROW WHEN NEW.predicate IS NOT NULL AND (
			instr(NEW.predicate, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: watches.predicate must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_events_ins
BEFORE INSERT ON webhooks
FOR EACH ROW WHEN NEW.events IS NOT NULL AND (
			instr(NEW.events, char(0)) > 0
			OR (json_valid(NEW.events) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.events)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.events must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_events_upd
BEFORE UPDATE OF events ON webhooks
FOR EACH ROW WHEN NEW.events IS NOT NULL AND (
			instr(NEW.events, char(0)) > 0
			OR (json_valid(NEW.events) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.events)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.events must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_secret_ins
BEFORE INSERT ON webhooks
FOR EACH ROW WHEN NEW.secret IS NOT NULL AND (
			instr(NEW.secret, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.secret must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_secret_upd
BEFORE UPDATE OF secret ON webhooks
FOR EACH ROW WHEN NEW.secret IS NOT NULL AND (
			instr(NEW.secret, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.secret must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_url_ins
BEFORE INSERT ON webhooks
FOR EACH ROW WHEN NEW.url IS NOT NULL AND (
			instr(NEW.url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_webhooks_url_upd
BEFORE UPDATE OF url ON webhooks
FOR EACH ROW WHEN NEW.url IS NOT NULL AND (
			instr(NEW.url, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: webhooks.url must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspace_invitations_email_ins
BEFORE INSERT ON workspace_invitations
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_invitations.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspace_invitations_email_upd
BEFORE UPDATE OF email ON workspace_invitations
FOR EACH ROW WHEN NEW.email IS NOT NULL AND (
			instr(NEW.email, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspace_invitations.email must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_description_ins
BEFORE INSERT ON workspaces
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_description_upd
BEFORE UPDATE OF description ON workspaces
FOR EACH ROW WHEN NEW.description IS NOT NULL AND (
			instr(NEW.description, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.description must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_name_ins
BEFORE INSERT ON workspaces
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_name_upd
BEFORE UPDATE OF name ON workspaces
FOR EACH ROW WHEN NEW.name IS NOT NULL AND (
			instr(NEW.name, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.name must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_settings_ins
BEFORE INSERT ON workspaces
FOR EACH ROW WHEN NEW.settings IS NOT NULL AND (
			instr(NEW.settings, char(0)) > 0
			OR (json_valid(NEW.settings) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.settings)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.settings must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_settings_upd
BEFORE UPDATE OF settings ON workspaces
FOR EACH ROW WHEN NEW.settings IS NOT NULL AND (
			instr(NEW.settings, char(0)) > 0
			OR (json_valid(NEW.settings) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.settings)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.settings must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_slug_ins
BEFORE INSERT ON workspaces
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.slug must not contain a NUL');
END;

CREATE TRIGGER IF NOT EXISTS pad_nul_workspaces_slug_upd
BEFORE UPDATE OF slug ON workspaces
FOR EACH ROW WHEN NEW.slug IS NOT NULL AND (
			instr(NEW.slug, char(0)) > 0
)
BEGIN
	SELECT RAISE(ABORT, 'pad_nul_invariant: workspaces.slug must not contain a NUL');
END;

