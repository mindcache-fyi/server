-- Full-text search index over mindcache briefs and main content.
-- modTime (RFC3339) tracks the indexed blob's modification time so the
-- reconciler can incrementally refresh stale entries without rescanning.
CREATE VIRTUAL TABLE IF NOT EXISTS MindcacheFTS USING fts5(
    mindcacheId UNINDEXED,
    modTime UNINDEXED,
    brief,
    content,
    tokenize='trigram'
);
