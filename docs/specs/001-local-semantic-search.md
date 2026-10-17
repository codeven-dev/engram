# Spec: Optional local semantic retrieval

## 1. Problem Statement & Goals

Engram's local FTS search can miss a relevant observation written in a different
language or vocabulary. Add optional multilingual candidates without changing
the MCP tools, result limits, project resolution or observation write protocol.

Goals:

- Combine lexical and semantic rankings behind existing search methods.
- Keep lexical search and durable saves working when the model is unavailable.
- Run inference locally through the already-installed Ollama runtime.
- Support Codex, Claude Code and Antigravity through their existing MCP contract.

Non-goals: automatic note fusion, new memory tools, raw transcript ingestion,
cloud vector search, automatic user configuration changes or remote deployment.

## 2. User Scenarios & Acceptance Criteria

- A Spanish query can retrieve an English observation, and conversely.
- A weak lexical hit does not prevent semantic retrieval.
- Disabling embeddings preserves the original search behavior.
- Missing models, invalid vectors, timeouts and unavailable Ollama degrade to
  lexical results without preventing saves.
- Project, scope, type and deletion filters apply before selecting candidates.
- Edits, imports, consolidation, sync and deletes invalidate indexed content;
  a stale in-flight embedding cannot become searchable after a newer edit.
- Long observations are chunked rather than silently truncated.
- Results remain bounded and deduplicated by observation ID; previews stay bounded.

## 3. Data Model & Schema Changes

The local observation remains the source of truth. An optional SQLite index adds:

| Table | Purpose |
| --- | --- |
| `semantic_pending` | Observation ID and invalidation version awaiting indexing. |
| `semantic_meta` | Model, dimensions and embedding recipe identifying this index. |
| `observation_vectors` | sqlite-vec chunk vectors linked to observation IDs. |

Ordinary SQL triggers enqueue changed IDs. Save transactions never run inference
or execute vector operations. Searches exclude pending IDs so stale vectors are
not returned while a model is offline. Publishing vectors and acknowledging the
same queue version occur in one transaction after checking the model identity.

## 4. API & Internal Interfaces

No new MCP tools or HTTP endpoints. Both `SearchContext` and
`SearchPreviewsContext` use the same candidate retrieval and rank fusion policy.
Existing exact topic matches keep priority. Lexical and semantic ranks are
combined by reciprocal rank, never by adding BM25 to cosine scores.

Environment configuration explicitly enables local inference:

- `ENGRAM_EMBED_MODEL`: Ollama model tag; unset disables inference.
- `ENGRAM_EMBED_URL`: loopback Ollama base URL, default `http://127.0.0.1:11434`.
- `ENGRAM_EMBED_DIMENSIONS`: index dimensions, default 768.
- `ENGRAM_EMBED_MIN_SIMILARITY`: semantic candidate threshold, evaluated against fixtures.

Long-lived MCP/HTTP stores perform bounded background indexing. Short-lived CLI
saves leave durable work for the next index pass. An explicit index/status command
supports manual setup and diagnosis. Runtime requests never download a model.

## 5. Processing Flow

Save observation and enqueue invalidation in SQLite; commit before inference.
Read a bounded queue batch, close its SQL cursor, embed chunks outside any write
transaction, then publish only if the queue version and model identity still match.
For search, obtain the existing lexical list and eligible semantic candidates,
deduplicate and fuse ranks, and return the existing number of results.

## 6. Implementation Phases (TDD Checklist)

- [x] Cross-language retrieval through the real store with a deterministic external provider.
- [x] Lexical fallback, scope isolation, invalidation and stale-publication regressions.
- [x] Long-note chunking, model configuration and index diagnostics.
- [x] Existing MCP preview integration and real local-model evaluation.
- [x] Manual three-agent guide and Antigravity lifecycle adapter contract tests.

## 7. Risks & Rollback Plan

Background indexing is eventually consistent. Until indexing completes, the note
is searchable lexically. Model tags must be pinned; replacing weights behind the
same tag requires rebuilding the index. Chunking increases disk and inference
cost, and semantic thresholds require project-specific evaluation.

Unset `ENGRAM_EMBED_MODEL` to restore lexical-only retrieval. Observations and
their sync/export contracts remain intact; vectors are local and rebuildable.
Upstream policies apply to upstream contributions; this experimental fork is
authorized by its owner for local evaluation and is not an upstream pull request.
