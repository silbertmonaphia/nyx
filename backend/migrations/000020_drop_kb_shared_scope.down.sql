ALTER TABLE kb_chunks ADD COLUMN scope TEXT NOT NULL DEFAULT 'user';
ALTER TABLE kb_chunks ALTER COLUMN scope DROP DEFAULT;
ALTER TABLE kb_chunks ADD CONSTRAINT kb_chunks_scope_check CHECK (scope IN ('shared', 'user'));
ALTER TABLE kb_chunks ADD CONSTRAINT kb_chunks_scope_owner CHECK (
    (scope = 'shared' AND owner_id IS NULL) OR
    (scope = 'user' AND owner_id IS NOT NULL)
);
ALTER TABLE kb_chunks ALTER COLUMN owner_id DROP NOT NULL;
DROP INDEX kb_chunks_owner_idx;
CREATE INDEX kb_chunks_scope_owner_idx ON kb_chunks (scope, owner_id);

DROP INDEX kb_documents_owner_rel_path_idx;
ALTER TABLE kb_documents ADD COLUMN scope TEXT NOT NULL DEFAULT 'user';
ALTER TABLE kb_documents ALTER COLUMN scope DROP DEFAULT;
ALTER TABLE kb_documents ADD CONSTRAINT kb_documents_scope_check CHECK (scope IN ('shared', 'user'));
ALTER TABLE kb_documents ADD CONSTRAINT kb_documents_scope_owner CHECK (
    (scope = 'shared' AND owner_id IS NULL) OR
    (scope = 'user' AND owner_id IS NOT NULL)
);
ALTER TABLE kb_documents ALTER COLUMN owner_id DROP NOT NULL;

ALTER TABLE kb_sources ADD COLUMN scope TEXT NOT NULL DEFAULT 'user';
ALTER TABLE kb_sources ALTER COLUMN scope DROP DEFAULT;
ALTER TABLE kb_sources ADD CONSTRAINT kb_sources_scope_check CHECK (scope IN ('shared', 'user'));
ALTER TABLE kb_sources ADD CONSTRAINT kb_sources_scope_owner CHECK (
    (scope = 'shared' AND owner_id IS NULL) OR
    (scope = 'user' AND owner_id IS NOT NULL)
);
ALTER TABLE kb_sources ALTER COLUMN owner_id DROP NOT NULL;
DROP INDEX kb_sources_owner_name_uidx;
CREATE UNIQUE INDEX kb_sources_scope_owner_name_uidx ON kb_sources (scope, COALESCE(owner_id, 0), name);
CREATE INDEX kb_sources_owner_idx ON kb_sources (owner_id) WHERE owner_id IS NOT NULL;
