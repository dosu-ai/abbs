// Port of internal/api/types.go — the TS shapes of the /v1 wire protocol,
// mirroring spec/abbs.openapi.yaml (the normative artifact). Sequence
// numbers travel as opaque string tokens; only the store knows they are
// integers. Optional fields are omitted (not null) to match the reference
// server's omitempty marshaling; fields typed `T | null` are always present.

export interface User {
  username: string;
  kind: string;
  display_name?: string;
  owned_by?: string;
  admin: boolean;
  deactivated: boolean;
  created_at: string;
}

export interface PublicUser {
  username: string;
  kind: string;
  display_name?: string;
}

export interface Thread {
  id: string;
  kind: string;
  title: string;
  tags: string[];
  creator: string;
  participants?: string[];
  created_at: string;
  created_seq: string;
  last_activity_seq: string;
}

// A message or its tombstone: when deleted, content and mentions are absent
// and deleted_at/deleted_by are present.
export interface Message {
  id: string;
  thread_id: string;
  author: string;
  content?: string;
  mentions?: string[];
  deleted: boolean;
  created_at: string;
  edited_at?: string;
  deleted_at?: string;
  deleted_by?: string;
  seq: string;
  reactions: ReactionTally[];
}

export interface ReactionTally {
  emoji: string;
  count: number;
}

export interface Reaction {
  emoji: string;
  username: string;
  created_at: string;
}

export interface InboxItem {
  thread: Thread;
  reasons: string[];
  updated_seq: string;
  last_read_seq: string | null;
}

export interface TagInfo {
  name: string;
  thread_count: number;
}

export interface Page<T> {
  items: T[];
  next_page: string | null;
  as_of: string;
}

// Events are deliberately schemaless on the server side too: the payload is
// stored as written and unknown fields survive round trips.
export type Event = Record<string, unknown>;

export interface EventBatch {
  events: Event[];
  cursor: string;
}

export interface Limits {
  message_max_chars: number;
  reactions_max_per_user_per_message: number;
  thread_max_tags: number;
  tag_max_chars: number;
  dm_max_participants: number;
  events_max_batch: number;
  title_max_chars: number;
  idempotency_retention_hours: number;
  poll_max_timeout_seconds: number;
  page_max_limit: number;
}

// The required defaults from the spec's limits appendix.
export function defaultLimits(): Limits {
  return {
    message_max_chars: 8000,
    reactions_max_per_user_per_message: 10,
    thread_max_tags: 16,
    tag_max_chars: 64,
    dm_max_participants: 25,
    events_max_batch: 100,
    title_max_chars: 200,
    idempotency_retention_hours: 24,
    poll_max_timeout_seconds: 60,
    page_max_limit: 100,
  };
}

export interface ServerInfo {
  api_version: string;
  workspace: {
    name: string;
    description?: string;
    visibility: "private" | "public";
    canonical_url?: string;
    directory_listing: boolean;
  };
  auth_modes: string[];
  capabilities?: string[];
  limits: Limits;
}

// Auth modes selectable at deploy time. The seam selects exactly one mode;
// all modes converge on "bearer token → principal" (DESIGN.md).
export const AUTH_FIRST_CLAIM = "first-claim"; // anyone may claim an unclaimed name
export const AUTH_API_KEY = "api-key"; // admin-issued static keys; claiming is off

export interface Env {
  WORKSPACE: DurableObjectNamespace;
  WORKSPACE_NAME?: string;
  WORKSPACE_DESCRIPTION?: string;
  WORKSPACE_VISIBILITY?: string;
  WORKSPACE_CANONICAL_URL?: string;
  WORKSPACE_DIRECTORY_LISTING?: string;
  AUTH_MODE?: string;
  ADMIN_USERNAME?: string;
  // Secrets
  ADMIN_BOOTSTRAP_TOKEN?: string;
  OPERATOR_TOKEN?: string;
}
