// Port of internal/store/idempotency.go — remembered writes: enough to
// detect body mismatches and replay the original response byte-for-byte.
// created_ms (not UnixNano) because nanoseconds exceed 2^53 and would
// corrupt the retention comparison as a JS number.

import { Store } from "./store";

export interface IdemRecord {
  requestHash: string;
  status: number;
  contentType: string;
  body: string;
}

// idemGet looks up a remembered (principal, endpoint, key) write no older
// than the retention horizon. null means no fresh record.
export function idemGet(
  s: Store,
  principal: string,
  endpoint: string,
  key: string,
  notBeforeMs: number,
): IdemRecord | null {
  const rows = s.sql
    .exec(
      `SELECT request_hash, status, content_type, body FROM idempotency
	 WHERE principal = ? AND endpoint = ? AND key = ? AND created_ms >= ?`,
      principal,
      endpoint,
      key,
      notBeforeMs,
    )
    .toArray();
  if (rows.length === 0) return null;
  const r = rows[0];
  return {
    requestHash: r.request_hash as string,
    status: r.status as number,
    contentType: r.content_type as string,
    body: r.body as string,
  };
}

// idemPut remembers a completed write and lazily purges expired records.
export function idemPut(
  s: Store,
  principal: string,
  endpoint: string,
  key: string,
  rec: IdemRecord,
  atMs: number,
  purgeBeforeMs: number,
): void {
  s.tx(() => {
    s.sql.exec(`DELETE FROM idempotency WHERE created_ms < ?`, purgeBeforeMs);
    s.sql.exec(
      `INSERT OR REPLACE INTO idempotency (principal, endpoint, key, request_hash, status, content_type, body, created_ms)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
      principal,
      endpoint,
      key,
      rec.requestHash,
      rec.status,
      rec.contentType,
      rec.body,
      atMs,
    );
  });
}
