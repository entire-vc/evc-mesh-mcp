# Conditional assigned-task snapshots

`get_my_tasks` returns complete descriptions, Unicode and ordinary comment
bodies supplied by the API. The default retains its lean item envelope; `full=true`
always returns the original REST response and bypasses all conditional behavior.
`list_tasks` and `poll_tasks` keep their existing contracts.

A versioned default cold response has the normal `tasks`, `count`,
`total_count`, `has_more` and other upstream envelope fields, plus `revision`.
The revision is an opaque SHA-256 digest. It covers canonical JSON and the
authenticated workspace/agent, API origin, transport session, effective filters,
limit, view, optional consumer cache namespace and protocol schema. It is never an authentication or lease token.
Every call first re-reads the authenticated API. Task versions include child
comment, artifact and VCS activity, including edits or deletion that leave
`updated_at` unchanged. An API without authoritative item versions returns
complete cold responses without advertising a revision. An incomplete upstream
page or description returns an explicit error instead of a reusable partial base.

Stateless HTTP callers must supply a non-secret `snapshot_session` namespace
for conditional responses. Generate a fresh namespace after consumer restart or
compaction. Without it, stateless HTTP always returns complete cold without a
revision. Stateful transports also isolate bases by their transport session;
`stdio` isolates by process lifetime. Never use credentials or lease tokens as
a namespace.

Send `known_revision` only while retaining the corresponding complete snapshot
for the same session, filters and view. An exact match returns only:

```json
{"revision":"<opaque digest>","unchanged":true}
```

Keep the previous snapshot unchanged. A mismatch returns a complete cold
response unless the caller also sets `accept_delta=true` and the server still
has that exact scoped base. An opt-in delta has this shape:

```json
{
  "delta": true,
  "base_revision": "<retained base>",
  "revision": "<new digest>",
  "changed": [{"id":"a","description":"Complete text","version":2}],
  "removed_ids": ["b"],
  "order": ["a"],
  "envelope": {"count":1,"total_count":1,"has_more":false}
}
```

`changed` contains complete rendered items, including every new item; each
changed item replaces the old item entirely. Delete `removed_ids`, replace/add
`changed`, then arrange the items in exactly `order`. Replace the entire
non-items envelope with `envelope`, including any unfamiliar fields. Store the
new `revision` separately. The reconstructed snapshot, excluding protocol
fields, must equal the canonical complete cold JSON exactly. Empty arrays are
arrays, not null. A page remains a page: preserve `has_more` and `total_count`.

Reject a delta without the matching complete base, an unknown schema, duplicate
or missing IDs, or an inconsistent order. After consumer cache loss, compaction,
restart or any reconstruction error, omit `known_revision` and request cold.
Do not send a naked revision retained without its snapshot. Server cache loss,
restart, scope mismatch, expiration or corrupt base also falls back to cold.
The server cache is capped at 128 bases and 32 MiB with a 30 minute TTL.
Snapshots larger than the byte cap are returned complete and never used as a base.

Transport size savings do not establish context savings: callers must measure
the actual text passed into their downstream context, including full requests,
retries and fallbacks. A caller that expands the complete snapshot into context
on every repeat does not save those context bytes.

The authoritative API version contract is implemented by
[`20261007002_task_version.sql`](https://git.entire.host/entire-vc/evc-mesh/-/blob/541621641b47d4a610c31b822143e61bd2628cc3/migrations/20261007002_task_version.sql):
every task update increments `version`, and insert/update/delete activity in
comments, artifacts and VCS links updates the parent task version without relying
on wall-clock timestamps. Apply that API migration before conditional rollout.

The 32 MiB bound covers retained snapshot bodies; each retained scope is a fixed
64-character digest, so large caller namespaces or filters cannot grow retained
scope memory. Entry count bounds fixed metadata overhead.

Lease capabilities are always omitted from model-facing cold/full/delta output.
The internal canonical snapshot remains unchanged; see [lease-output.md](lease-output.md).
