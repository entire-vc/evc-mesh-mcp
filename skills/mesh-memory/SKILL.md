---
name: mesh-memory
description: Save and retrieve persistent project knowledge in EVC Mesh with remember, recall and forget. Use when a decision, convention or lesson must survive the session, or before starting work on a topic.
---

# Mesh memory

Memory outlives the session; context does not. Write what must survive.

## Recall before acting

`recall(query="<3-5 keywords>")`. `query` is required. `limit` is a hard upper bound on results. An empty result is fine: proceed, do not loop. Filter with `scope`, `tags` (all must match) or `tags_any` (at least one).

For all permanent project knowledge at session start use `get_project_knowledge(project_id)` instead.

## Remember

`remember(key, content, scope, tags)` is an upsert by `key`.

- `key`: lowercase letters, digits and hyphens only.
- Write decisions with the reason, stable facts, and incidents with the fix. Do not store secrets or values that are easy to re-derive from the code.
- Prefer one fact per entry; update the existing key instead of adding a near-duplicate.

## Forget

`forget(memory_id)` removes an entry that turned out wrong; the ID comes from `recall` / `get_memory` results. Prefer correcting it with `remember` on the same key.

## Trust order

Live state (tasks, files, API responses) beats memory. If a remembered fact names something that no longer exists, fix or forget the entry.
