-- Pad PostgreSQL schema (consolidated from SQLite migrations 001-021)
-- This is the initial schema for PostgreSQL deployments.

-- Enable UUID generation
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ========== Core tables ==========

CREATE TABLE IF NOT EXISTS workspaces (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    settings    JSONB NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    deleted_at  TEXT
);

CREATE TABLE IF NOT EXISTS documents (
    id               TEXT PRIMARY KEY,
    workspace_id     TEXT NOT NULL REFERENCES workspaces(id),
    title            TEXT NOT NULL,
    slug             TEXT NOT NULL,
    content          TEXT NOT NULL DEFAULT '',
    doc_type         TEXT NOT NULL DEFAULT 'notes'
                     CHECK (doc_type IN ('roadmap','phase-plan','architecture','ideation',
                                         'feature-spec','notes','prompt-library','reference')),
    status           TEXT NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft','active','completed','archived')),
    tags             JSONB NOT NULL DEFAULT '[]',
    pinned           BOOLEAN NOT NULL DEFAULT FALSE,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL DEFAULT 'user'
                     CHECK (created_by IN ('user','agent')),
    last_modified_by TEXT NOT NULL DEFAULT 'user'
                     CHECK (last_modified_by IN ('user','agent')),
    source           TEXT NOT NULL DEFAULT 'web'
                     CHECK (source IN ('cli','web','skill')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    deleted_at       TEXT,

    -- Full-text search vector (auto-updated via trigger)
    search_vector    TSVECTOR,

    UNIQUE(workspace_id, slug),
    UNIQUE(workspace_id, title)
);

CREATE INDEX IF NOT EXISTS idx_documents_workspace ON documents(workspace_id);
CREATE INDEX IF NOT EXISTS idx_documents_type ON documents(workspace_id, doc_type);
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_documents_updated ON documents(workspace_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_documents_fts ON documents USING GIN(search_vector);

-- Trigger to maintain document search vector
CREATE OR REPLACE FUNCTION documents_search_vector_update() RETURNS TRIGGER AS $$
BEGIN
    NEW.search_vector :=
        setweight(to_tsvector('english', COALESCE(NEW.title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(NEW.content, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(NEW.tags::text, '')), 'C');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER documents_search_vector_trigger
    BEFORE INSERT OR UPDATE OF title, content, tags ON documents
    FOR EACH ROW EXECUTE FUNCTION documents_search_vector_update();

CREATE TABLE IF NOT EXISTS versions (
    id              TEXT PRIMARY KEY,
    document_id     TEXT NOT NULL REFERENCES documents(id),
    content         TEXT NOT NULL,
    change_summary  TEXT NOT NULL DEFAULT '',
    is_diff         BOOLEAN NOT NULL DEFAULT FALSE,
    created_by      TEXT NOT NULL DEFAULT 'user'
                    CHECK (created_by IN ('user','agent')),
    source          TEXT NOT NULL DEFAULT 'web'
                    CHECK (source IN ('cli','web','skill')),
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_versions_document ON versions(document_id, created_at);

CREATE TABLE IF NOT EXISTS activities (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    document_id   TEXT,
    action        TEXT NOT NULL
                  CHECK (action IN ('created','updated','archived',
                                    'restored','read','searched')),
    actor         TEXT NOT NULL CHECK (actor IN ('user','agent')),
    source        TEXT NOT NULL CHECK (source IN ('cli','web','skill')),
    metadata      JSONB NOT NULL DEFAULT '{}',
    user_id       TEXT,
    created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_activities_workspace ON activities(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_activities_document ON activities(document_id, created_at);

-- ========== Collections & Items ==========

CREATE TABLE IF NOT EXISTS collections (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL,
    icon         TEXT DEFAULT '',
    description  TEXT DEFAULT '',
    prefix       TEXT NOT NULL DEFAULT '',
    schema       JSONB NOT NULL DEFAULT '{"fields":[]}',
    settings     JSONB DEFAULT '{}',
    sort_order   INTEGER DEFAULT 0,
    is_default   BOOLEAN DEFAULT FALSE,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    deleted_at   TEXT,
    UNIQUE(workspace_id, slug)
);

CREATE TABLE IF NOT EXISTS items (
    id                      TEXT PRIMARY KEY,
    workspace_id            TEXT NOT NULL REFERENCES workspaces(id),
    collection_id           TEXT NOT NULL REFERENCES collections(id),
    title                   TEXT NOT NULL,
    slug                    TEXT NOT NULL,
    content                 TEXT DEFAULT '',
    fields                  JSONB DEFAULT '{}',
    tags                    JSONB DEFAULT '[]',
    pinned                  BOOLEAN DEFAULT FALSE,
    sort_order              INTEGER DEFAULT 0,
    item_number             INTEGER,
    parent_id               TEXT REFERENCES items(id),
    created_by              TEXT DEFAULT 'user',
    last_modified_by        TEXT DEFAULT 'user',
    source                  TEXT DEFAULT 'web',
    created_by_user_id      TEXT,
    last_modified_by_user_id TEXT,
    assigned_user_id        TEXT,
    agent_role_id           TEXT,
    role_sort_order         INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    deleted_at              TEXT,

    -- Full-text search vector
    search_vector           TSVECTOR,

    UNIQUE(workspace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_items_collection ON items(collection_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_workspace ON items(workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_updated ON items(updated_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_assigned_user ON items(assigned_user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_agent_role ON items(agent_role_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_items_fts ON items USING GIN(search_vector);

-- Trigger to maintain item search vector
CREATE OR REPLACE FUNCTION items_search_vector_update() RETURNS TRIGGER AS $$
BEGIN
    NEW.search_vector :=
        setweight(to_tsvector('english', COALESCE(NEW.title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(NEW.content, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(NEW.tags::text, '')), 'C');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER items_search_vector_trigger
    BEFORE INSERT OR UPDATE OF title, content, tags ON items
    FOR EACH ROW EXECUTE FUNCTION items_search_vector_update();

CREATE TABLE IF NOT EXISTS item_links (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    source_id    TEXT NOT NULL REFERENCES items(id),
    target_id    TEXT NOT NULL REFERENCES items(id),
    link_type    TEXT DEFAULT 'related',
    created_by   TEXT DEFAULT 'user',
    user_id      TEXT,
    created_at   TEXT NOT NULL,
    UNIQUE(source_id, target_id, link_type)
);

CREATE INDEX IF NOT EXISTS idx_links_source ON item_links(source_id);
CREATE INDEX IF NOT EXISTS idx_links_target ON item_links(target_id);

CREATE TABLE IF NOT EXISTS views (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    collection_id TEXT REFERENCES collections(id),
    name          TEXT NOT NULL,
    slug          TEXT NOT NULL,
    view_type     TEXT NOT NULL,
    config        JSONB DEFAULT '{}',
    sort_order    INTEGER DEFAULT 0,
    is_default    BOOLEAN DEFAULT FALSE,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE(workspace_id, slug)
);

CREATE TABLE IF NOT EXISTS item_versions (
    id             TEXT PRIMARY KEY,
    item_id        TEXT NOT NULL REFERENCES items(id),
    content        TEXT NOT NULL,
    change_summary TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT 'user',
    source         TEXT NOT NULL DEFAULT 'web',
    is_diff        BOOLEAN NOT NULL DEFAULT FALSE,
    user_id        TEXT,
    created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_item_versions_item ON item_versions(item_id, created_at);

-- ========== Comments & Reactions ==========

CREATE TABLE IF NOT EXISTS comments (
    id           TEXT PRIMARY KEY,
    item_id      TEXT NOT NULL REFERENCES items(id),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    author       TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    user_id      TEXT,
    activity_id  TEXT,
    parent_id    TEXT REFERENCES comments(id),
    created_by   TEXT NOT NULL DEFAULT 'user'
                 CHECK (created_by IN ('user', 'agent')),
    source       TEXT NOT NULL DEFAULT 'web'
                 CHECK (source IN ('cli', 'web', 'skill')),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,

    -- Full-text search vector
    search_vector TSVECTOR
);

CREATE INDEX IF NOT EXISTS idx_comments_item ON comments(item_id, created_at);
CREATE INDEX IF NOT EXISTS idx_comments_workspace ON comments(workspace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_comments_parent ON comments(parent_id);
CREATE INDEX IF NOT EXISTS idx_comments_activity ON comments(activity_id);
CREATE INDEX IF NOT EXISTS idx_comments_fts ON comments USING GIN(search_vector);

-- Trigger to maintain comment search vector
CREATE OR REPLACE FUNCTION comments_search_vector_update() RETURNS TRIGGER AS $$
BEGIN
    NEW.search_vector :=
        to_tsvector('english', COALESCE(NEW.body, ''));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER comments_search_vector_trigger
    BEFORE INSERT OR UPDATE OF body ON comments
    FOR EACH ROW EXECUTE FUNCTION comments_search_vector_update();

CREATE TABLE IF NOT EXISTS comment_reactions (
    id         TEXT PRIMARY KEY,
    comment_id TEXT NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    user_id    TEXT,
    actor      TEXT NOT NULL DEFAULT 'user',
    emoji      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(comment_id, user_id, emoji)
);

CREATE INDEX IF NOT EXISTS idx_comment_reactions_comment ON comment_reactions(comment_id);

-- ========== Users & Auth ==========

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('admin', 'member')),
    avatar_url    TEXT DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    updated_at    TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    token_hash  TEXT NOT NULL,
    device_info TEXT DEFAULT '',
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_token_hash ON sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    role         TEXT NOT NULL DEFAULT 'editor'
                 CHECK (role IN ('owner', 'editor', 'viewer')),
    created_at   TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    PRIMARY KEY (workspace_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_workspace_members_user ON workspace_members(user_id);

CREATE TABLE IF NOT EXISTS workspace_invitations (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    email        TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'editor'
                 CHECK (role IN ('owner', 'editor', 'viewer')),
    invited_by   TEXT NOT NULL REFERENCES users(id),
    code         TEXT NOT NULL UNIQUE,
    accepted_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS idx_invitations_workspace ON workspace_invitations(workspace_id);
CREATE INDEX IF NOT EXISTS idx_invitations_code ON workspace_invitations(code);
CREATE INDEX IF NOT EXISTS idx_invitations_email ON workspace_invitations(email);

-- ========== API Tokens ==========

CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    user_id      TEXT REFERENCES users(id),
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    scopes       JSONB NOT NULL DEFAULT '["*"]',
    expires_at   TEXT,
    last_used_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

-- ========== Webhooks ==========

CREATE TABLE IF NOT EXISTS webhooks (
    id                TEXT PRIMARY KEY,
    workspace_id      TEXT NOT NULL REFERENCES workspaces(id),
    url               TEXT NOT NULL,
    secret            TEXT DEFAULT '',
    events            JSONB NOT NULL DEFAULT '["*"]',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    updated_at        TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT,
    last_triggered_at TEXT,
    failure_count     INTEGER NOT NULL DEFAULT 0
);

-- ========== Agent Roles ==========

CREATE TABLE IF NOT EXISTS agent_roles (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    slug         TEXT NOT NULL,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    icon         TEXT NOT NULL DEFAULT '',
    tools        TEXT NOT NULL DEFAULT '',
    sort_order   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE(workspace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_agent_roles_workspace ON agent_roles(workspace_id);

-- Foreign keys for items that reference users/agent_roles (added after tables exist)
ALTER TABLE items ADD CONSTRAINT fk_items_created_by_user FOREIGN KEY (created_by_user_id) REFERENCES users(id);
ALTER TABLE items ADD CONSTRAINT fk_items_modified_by_user FOREIGN KEY (last_modified_by_user_id) REFERENCES users(id);
ALTER TABLE items ADD CONSTRAINT fk_items_assigned_user FOREIGN KEY (assigned_user_id) REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE items ADD CONSTRAINT fk_items_agent_role FOREIGN KEY (agent_role_id) REFERENCES agent_roles(id) ON DELETE SET NULL;
ALTER TABLE comments ADD CONSTRAINT fk_comments_activity FOREIGN KEY (activity_id) REFERENCES activities(id);

-- ========== Platform Settings ==========

CREATE TABLE IF NOT EXISTS platform_settings (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

-- ========== Password Resets ==========

CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    token_hash  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT,
    created_at  TEXT NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC')::TEXT
);

CREATE INDEX IF NOT EXISTS idx_reset_tokens_token_hash ON password_reset_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_reset_tokens_user_id ON password_reset_tokens(user_id);

-- ========== Legacy tables (kept for compatibility) ==========

CREATE TABLE IF NOT EXISTS custom_templates (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    doc_type     TEXT NOT NULL DEFAULT 'notes',
    icon         TEXT NOT NULL DEFAULT '📝',
    content      TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE(workspace_id, name)
);

CREATE TABLE IF NOT EXISTS progress_snapshots (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id),
    total_tasks  INTEGER NOT NULL DEFAULT 0,
    done_tasks   INTEGER NOT NULL DEFAULT 0,
    open_tasks   INTEGER NOT NULL DEFAULT 0,
    in_progress  INTEGER NOT NULL DEFAULT 0,
    percentage   REAL NOT NULL DEFAULT 0.0,
    phase_data   JSONB NOT NULL DEFAULT '[]',
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshots_workspace_time
    ON progress_snapshots(workspace_id, created_at);

-- ========== Migration tracking ==========

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);
