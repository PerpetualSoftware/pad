CREATE TABLE workspace_slug_claims (
    slug TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL
);
INSERT INTO workspace_slug_claims (slug, workspace_id) SELECT slug, id FROM workspaces;
CREATE INDEX workspace_slug_claims_workspace_id ON workspace_slug_claims(workspace_id);

CREATE TRIGGER workspace_slug_claim_insert BEFORE INSERT ON workspaces BEGIN
    SELECT RAISE(IGNORE) WHERE EXISTS (
        SELECT 1 FROM workspace_slug_claims WHERE slug = NEW.slug AND workspace_id <> NEW.id
    );
    INSERT INTO workspace_slug_claims (slug, workspace_id)
        VALUES (NEW.slug, NEW.id) ON CONFLICT (slug) DO NOTHING;
END;
CREATE TRIGGER workspace_slug_claim_update BEFORE UPDATE OF slug ON workspaces WHEN NEW.slug <> OLD.slug BEGIN
    SELECT RAISE(ABORT, 'workspace slug conflict') WHERE EXISTS (
        SELECT 1 FROM workspace_slug_claims WHERE slug = NEW.slug AND workspace_id <> NEW.id
    );
    INSERT INTO workspace_slug_claims (slug, workspace_id)
        VALUES (NEW.slug, NEW.id) ON CONFLICT (slug) DO NOTHING;
END;
CREATE TRIGGER workspace_slug_claim_delete AFTER DELETE ON workspaces BEGIN
    DELETE FROM workspace_slug_claims WHERE workspace_id = OLD.id;
END;
