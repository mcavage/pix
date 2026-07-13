---
name: api-conventions
description: REST API design and review patterns. Use when designing, building, or reviewing a REST API, or when asked "how should this endpoint work", "what status code", "how do I paginate", "review my API design", or "does this follow conventions".
---
# api-conventions

Iron laws for REST API design. Apply these when drafting or reviewing endpoints.
When the API surface is large or fuzzy, run `build` first to anchor requirements.
Run `code-review` on the implementation before shipping.

## URLs and methods

- Plural nouns, no verbs: `/users`, `/orders`, `/projects/{id}/items`.
- Hierarchy only when the sub-resource never exists outside the parent.
  Flat is usually cleaner beyond two levels.
- `GET` reads (idempotent, no body). `POST` creates. `PUT` full replace.
  `PATCH` partial update. `DELETE` removes.
- Actions that don't map to CRUD: use a sub-resource noun, not a verb.
  `POST /payments/{id}/refunds`, not `POST /payments/{id}/refund`.

## Request and response shape

All responses use a consistent envelope:

```json
{ "data": {}, "meta": {} }
{ "data": [], "meta": { "cursor": "x", "has_more": true } }
{ "error": { "code": "validation_failed", "message": "human-readable", "details": [] } }
```

- `data` is always an object or array; never a bare primitive at the top level.
- `meta` carries pagination, request IDs, and nothing else.
- Error `code` is snake_case and machine-stable. `message` is for humans.
  `details` is an array of field-level errors: `[{"field":"email","message":"..."}]`.
- No null fields in the response. Omit absent optional fields.

## Pagination

Cursor-based by default: `?cursor=<opaque>&limit=50`. Max limit enforced server-side.
Offset pagination (`?page=N&per_page=N`) only when the client genuinely needs
random-access jumps (admin UIs, exports). Never mix both on the same collection.

## Status codes

| Range | Use |
|-------|-----|
| 200 | success with body |
| 201 | created (include `Location` header) |
| 204 | success, no body (DELETE, certain PATCHes) |
| 400 | bad input (always include `error.details`) |
| 401 | unauthenticated |
| 403 | authenticated but not allowed |
| 404 | resource not found |
| 409 | conflict (duplicate, stale write) |
| 422 | valid syntax, failed business validation |
| 429 | rate limited (include `Retry-After`) |
| 500 | unhandled server error |

Never return 200 with an error body. Never return 500 for a client mistake.

## Auth and keys

- Bearer token in `Authorization` header. Never in query params.
- API keys: prefix them (`sk_live_...`, `sk_test_...`) so secrets scanners catch leaks.
- Never log tokens, keys, or any `Authorization` header value.

## Versioning and evolution

- Version in the path: `/v1/`, `/v2/`. No version in the hostname.
- Additive changes (new fields, new optional params) are non-breaking; do them freely.
- Removing or renaming fields, changing types, making optional params required:
  bump the major version and keep the old version alive through a sunset period.
- Include a `Sunset` header on deprecated endpoints.

## Review checklist

Before shipping an API change, confirm:
- Every new endpoint has a corresponding integration test covering the 4xx paths.
- Breaking changes are version-gated, not silent.
- No sensitive data in URLs (tokens, SSNs, PII belong in the body or headers).
- Rate limits and auth are applied at the router level, not inside each handler.
