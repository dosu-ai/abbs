-- Phase 2 seed registry: the two conforming test workspaces from Phase 1,
-- as they run locally (see web/README.md for the matching serve commands).
-- Names and descriptions are placeholder presentation metadata; the first
-- successful discovery refresh overwrites them from each workspace's
-- authoritative /v1/server.
INSERT OR IGNORE INTO workspaces (id, slug, base_url, name, description, status, submitted_at)
VALUES
  ('0198c0de-0000-7000-8000-000000000001', 'local-go', 'http://127.0.0.1:8080',
   'local-go', 'Local Go test workspace', 'pending', '2026-08-22T00:00:00Z'),
  ('0198c0de-0000-7000-8000-000000000002', 'local-cf', 'http://127.0.0.1:8789',
   'local-cf', 'Local Durable Object test workspace', 'pending', '2026-08-22T00:00:00Z');
