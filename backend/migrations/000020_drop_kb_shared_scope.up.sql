-- safe-migration: shared KB rows are deleted by design in the same release that contracts the now one-valued scope columns; KB is default-off and migrations run at boot before requests are served.
DELETE FROM kb_sources WHERE scope = 'shared';

DROP INDEX kb_sources_scope_owner_name_uidx;
ALTER TABLE kb_sources DROP CONSTRAINT kb_sources_scope_owner;
ALTER TABLE kb_sources DROP COLUMN scope;
ALTER TABLE kb_sources ALTER COLUMN owner_id SET NOT NULL;
DROP INDEX kb_sources_owner_idx;
CREATE UNIQUE INDEX kb_sources_owner_name_uidx ON kb_sources (owner_id, name);

ALTER TABLE kb_documents DROP CONSTRAINT kb_documents_scope_owner;
ALTER TABLE kb_documents DROP COLUMN scope;
ALTER TABLE kb_documents ALTER COLUMN owner_id SET NOT NULL;
CREATE INDEX kb_documents_owner_rel_path_idx ON kb_documents (owner_id, rel_path);

ALTER TABLE kb_chunks DROP CONSTRAINT kb_chunks_scope_owner;
DROP INDEX kb_chunks_scope_owner_idx;
ALTER TABLE kb_chunks DROP COLUMN scope;
ALTER TABLE kb_chunks ALTER COLUMN owner_id SET NOT NULL;
CREATE INDEX kb_chunks_owner_idx ON kb_chunks (owner_id);
