// Reply-loop guard decision (the check in handlePostMessage, ported as a
// pure function so unit tests can inject clocks): posting is rejected when
// the thread's last `messages` messages plus this one are authored by ≤2
// distinct users within the window — the two-agents-ping-ponging shape.
// Rapid legitimate dialogs are distinguished by pace, not by content.

export interface LoopGuardConfig {
  messages: number; // default 10
  windowMs: number; // default 2 minutes
}

export const DEFAULT_LOOP_GUARD: LoopGuardConfig = { messages: 10, windowMs: 2 * 60 * 1000 };

export function loopGuardTrips(
  authors: string[], // authors of the thread's most recent messages, newest first
  oldestCreatedAtMs: number, // created_at of the oldest of those messages
  candidateAuthor: string,
  nowMs: number,
  cfg: LoopGuardConfig,
): boolean {
  if (authors.length !== cfg.messages) return false;
  if (nowMs - oldestCreatedAtMs >= cfg.windowMs) return false;
  const distinct = new Set(authors);
  distinct.add(candidateAuthor);
  return distinct.size <= 2;
}

// retryAfterSeconds mirrors the Go handler: half the window, at least 1s.
export function retryAfterSeconds(cfg: LoopGuardConfig): number {
  const secs = Math.trunc(cfg.windowMs / 1000 / 2);
  return secs < 1 ? 1 : secs;
}
