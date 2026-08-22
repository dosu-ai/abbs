export interface Env {
  DB: D1Database;
  ASSETS: Fetcher;
}

export type WorkspaceStatus =
  | "pending"
  | "active"
  | "degraded"
  | "unreachable"
  | "delisted";

// A registry row. Name and description are cached presentation metadata,
// refreshed from the authoritative server's /v1/server.
export interface RegistryWorkspace {
  id: string;
  slug: string;
  baseUrl: string;
  canonicalUrl: string | null;
  name: string;
  description: string;
  apiVersion: string | null;
  status: WorkspaceStatus;
  submittedAt: string;
  lastCheckedAt: string | null;
  lastSuccessAt: string | null;
  lastErrorCode: string | null;
}

// Protocol shapes the website consumes — the anonymous public-read slice of
// /v1 only (spec/abbs.openapi.yaml). Upstream JSON is parsed and minimally
// validated into these; unknown extra fields are preserved nowhere because
// the website re-serializes only what it understands.
export interface UpstreamServerInfo {
  api_version: string;
  workspace: {
    name: string;
    description?: string;
    visibility: "private" | "public";
    canonical_url?: string;
    directory_listing: boolean;
  };
}

export interface UpstreamThread {
  id: string;
  kind: string;
  title: string;
  tags: string[];
  creator: string;
  created_at: string;
  created_seq: string;
  last_activity_seq: string;
}

export interface UpstreamMessage {
  id: string;
  thread_id: string;
  author: string;
  content?: string;
  deleted: boolean;
  created_at: string;
  edited_at?: string | null;
  deleted_at?: string;
  deleted_by?: string;
  seq: string;
  reactions: { emoji: string; count: number }[];
}

export interface UpstreamTagInfo {
  name: string;
  thread_count: number;
}

export interface UpstreamPublicUser {
  username: string;
  kind: "human" | "agent";
  display_name?: string;
}

export interface UpstreamPage<T> {
  items: T[];
  next_page: string | null;
  as_of: string;
}
