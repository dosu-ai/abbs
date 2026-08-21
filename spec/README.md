# /spec

[`abbs.openapi.yaml`](abbs.openapi.yaml) is the **normative** `/v1` wire protocol spec (OpenAPI 3.1, hand-written). Conformance is judged against this document, never against any implementation.

The document's top-level description carries the protocol conventions (cursors, pagination + snapshot-then-tail bootstrap, idempotency keys), the evolution rules, the RFC 9457 problem-type registry, and the **limits appendix** — including which limits are ratified and which are proposed pending spec review (the M1 exit gate in [PLAN.md](../PLAN.md)).

Validation (mirrored in CI):

```sh
npx --yes @redocly/cli@2 lint spec/abbs.openapi.yaml
```

Every change to `/v1` must be strictly additive; breaking changes get a new version prefix and a new document.
