CREATE TABLE workspace_slug_claims (
    slug TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL
);
INSERT INTO workspace_slug_claims (slug, workspace_id) SELECT slug, id FROM workspaces;
CREATE INDEX workspace_slug_claims_workspace_id ON workspace_slug_claims(workspace_id);

CREATE FUNCTION workspace_slug_claim() RETURNS trigger AS $$
BEGIN
    INSERT INTO workspace_slug_claims (slug, workspace_id) VALUES (NEW.slug, NEW.id)
        ON CONFLICT (slug) DO NOTHING;
    IF NOT EXISTS (SELECT 1 FROM workspace_slug_claims WHERE slug = NEW.slug AND workspace_id = NEW.id) THEN
        IF TG_OP = 'INSERT' THEN RETURN NULL; END IF;
        RAISE EXCEPTION 'workspace slug conflict';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER workspace_slug_claim_insert BEFORE INSERT ON workspaces FOR EACH ROW EXECUTE FUNCTION workspace_slug_claim();
CREATE TRIGGER workspace_slug_claim_update BEFORE UPDATE OF slug ON workspaces FOR EACH ROW WHEN (NEW.slug <> OLD.slug) EXECUTE FUNCTION workspace_slug_claim();
CREATE FUNCTION workspace_slug_claim_purge() RETURNS trigger AS $$
BEGIN
    DELETE FROM workspace_slug_claims WHERE workspace_id = OLD.id
        AND (SELECT COUNT(*) FROM workspace_slug_claims WHERE workspace_id = OLD.id) = 1;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER workspace_slug_claim_delete AFTER DELETE ON workspaces FOR EACH ROW EXECUTE FUNCTION workspace_slug_claim_purge();
