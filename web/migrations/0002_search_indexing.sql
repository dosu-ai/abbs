-- Search indexing is deliberately opt-in twice over: directory consent must
-- remain valid, and two scheduled checks plus a visible public message are
-- required before a workspace can enter crawler-facing URL inventories.
-- Existing and newly registered rows therefore start unqualified.
ALTER TABLE workspaces ADD COLUMN search_eligible INTEGER NOT NULL DEFAULT 0
  CHECK (search_eligible IN (0, 1));
ALTER TABLE workspaces ADD COLUMN search_success_count INTEGER NOT NULL DEFAULT 0
  CHECK (search_success_count BETWEEN 0 AND 2);
ALTER TABLE workspaces ADD COLUMN search_eligible_at TEXT;
ALTER TABLE workspaces ADD COLUMN search_content_found INTEGER NOT NULL DEFAULT 0
  CHECK (search_content_found IN (0, 1));
ALTER TABLE workspaces ADD COLUMN inventory_phase TEXT NOT NULL DEFAULT 'bootstrap'
  CHECK (inventory_phase IN ('bootstrap', 'catchup', 'incremental'));
ALTER TABLE workspaces ADD COLUMN inventory_cursor TEXT;
ALTER TABLE workspaces ADD COLUMN inventory_anchor TEXT;
ALTER TABLE workspaces ADD COLUMN inventory_completed_at TEXT;

-- Narrow persistence exception for crawler discovery. This table contains
-- stable URL identifiers only: never titles, authors, messages, or events.
CREATE TABLE public_thread_urls (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  thread_id TEXT NOT NULL,
  discovered_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, thread_id)
);

CREATE INDEX public_thread_urls_by_workspace_seen
  ON public_thread_urls(workspace_id, last_seen_at, thread_id);
