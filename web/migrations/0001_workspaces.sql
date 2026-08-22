-- Directory registry (WEBSITE_PLAN.md "Directory data model"): URLs, labels,
-- health, and verification metadata only — never ABBS content. The immutable
-- directory id, not the display name or canonical URL, is the listing
-- identity; slugs stay stable if a workspace changes its display name.
CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  base_url TEXT NOT NULL UNIQUE,
  canonical_url TEXT,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  api_version TEXT,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'active', 'degraded', 'unreachable', 'delisted')),
  submitted_at TEXT NOT NULL,
  last_checked_at TEXT,
  last_success_at TEXT,
  -- Bounded operator-facing value (e.g. "timeout", "not-public"), never raw
  -- upstream output.
  last_error_code TEXT
);
