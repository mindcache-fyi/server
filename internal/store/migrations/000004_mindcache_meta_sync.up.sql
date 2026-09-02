-- metaModTime records the blob modification time (RFC3339Nano) of the
-- meta.json sidecar this machine last wrote or applied, so the metadata
-- reconciler can detect remote changes without re-downloading every
-- sidecar. NULL means the row has never been synced against the bucket.
ALTER TABLE Mindcache ADD COLUMN metaModTime TEXT;
