# Optional local semantic retrieval

This fork adds local multilingual candidates to Engram's existing search. Enable
Ollama embeddings explicitly; without them, Engram keeps its lexical behavior.
The MCP tools, project resolver, curated notes and result limits remain unchanged.

Start with the [manual three-agent guide](runbooks/manual-local-memory.md).
The implementation scope is recorded in the [specification](specs/001-local-semantic-search.md).

## Retrieval contract

| Step | Behavior |
| --- | --- |
| Save | Commit the observation and a small invalidation record in SQLite. |
| Index | A background worker reads title and content and embeds Unicode-safe overlapping chunks. |
| Search | Embed the query supplied to `mem_search`, CLI search or local HTTP search. |
| Select | Apply existing project, scope, type and deletion filters before limiting candidates. |
| Combine | Fuse lexical and semantic positions with reciprocal rank; retain exact topic priority. |
| Respond | Return the existing bounded previews and IDs; the agent can request full observations. |

No full conversation, hidden reasoning or tool transcript is sent to the embedding
model. There is no new automatic retrieval on every user message. The agent still
decides when to use Engram, including searches during a long tool loop.

FTS remains useful for exact identifiers. The semantic branch can add a relevant
candidate even when FTS returns a weak hit. Both lists are bounded by the normal
search limit and the fused response keeps that limit. BM25 and cosine are not
added together because their numeric scales are unrelated. `match_mode` controls
the lexical branch; enabled hybrid retrieval can add semantic candidates that do
not satisfy its literal terms.

## Local inference and failure behavior

The initial tested provider is Ollama's local `/api/embed`, using
`embeddinggemma:300m-qat-q4_0`, 768 dimensions. The adapter accepts only HTTP
loopback endpoints and rejects redirects. No model is downloaded during a save,
search or startup request. No Python runtime is added to Engram.

| Configuration | Default |
| --- | --- |
| `ENGRAM_EMBED_MODEL` | Unset: lexical only |
| `ENGRAM_EMBED_URL` | `http://127.0.0.1:11434` |
| `ENGRAM_EMBED_DIMENSIONS` | 768 |
| `ENGRAM_EMBED_MIN_SIMILARITY` | 0.35 |

`scripts/local-memory.sh` is an explicit opt-in shell configuration with the
quantized model and isolated `~/.engram-codeven` directory. It does not modify
agent settings. It should be sourced from this checkout's root after building.

Semantic queries have a two-second budget. Index passes have a thirty-second
budget and process at most eight observations; pending work rotates by last
attempt so an unembeddable note does not starve later notes. Long-lived stores
run a pass approximately once per second after the preceding pass completes.
`engram embeddings index` processes pending batches for up to five minutes.

Missing models, unavailable Ollama, malformed output, zero vectors, incorrect
dimensions and timeouts leave observations durable. Queries return their lexical
results. Invalid embedding configuration disables semantic retrieval. Diagnostics
go to stderr with fixed reasons, without note or query text. A cold model can
take longer than the query budget to load, so the first search may be lexical
while subsequent searches benefit from the warmed model.

The threshold is experimental, not a probability that a memory is correct.
Evaluate retrieval and irrelevant candidates on your own project before tuning
it. A missing relevant memory can also mean it has not been indexed yet.

## SQLite additions

| Table | Fields | Purpose |
| --- | --- | --- |
| `semantic_pending` | `observation_id INTEGER PRIMARY KEY`, `version TEXT`, `attempted_at INTEGER` | Durable invalidations and fair retry ordering. |
| `semantic_meta` | `id INTEGER PRIMARY KEY CHECK(id=1)`, `model_key TEXT` | Identity of the configured model, dimensions, endpoint and chunk recipe. |
| `observation_vectors` | Internal row ID, `observation_id INTEGER`, `embedding FLOAT[N] distance_metric=cosine` | One vector per note chunk in a sqlite-vec virtual table. |

sqlite-vec also creates its own shadow tables. Existing reserved embedding fields
on `observations` are left untouched. This change adds no proposal table and no
MCP tool.

Triggers on inserts, relevant edits and deletes only write ordinary queue rows.
They never call the model or operate on vector tables inside a save transaction.
The same invalidation path therefore covers CLI/MCP/HTTP saves, topic updates,
imports, consolidation, sync changes and deletion of observations.

Every invalidation gets a fresh random version token. Publishing checks that token
and the model identity in a transaction, then replaces vectors and acknowledges
exactly that version. This prevents older work from reappearing after another
process indexed and a later edit arrived. Pending observations are excluded from
vector retrieval, and hydration rechecks eligibility and selectors.

Chunking uses 512 Unicode code points with 64 points of overlap. All chunks are
included; inference uses `truncate:false`. A chunk that the configured model still
cannot accept leaves the note pending and available through FTS rather than
silently indexing an incomplete prefix.

The current vector query is an exact scan using sqlite-vec cosine distance,
aggregated by observation before `LIMIT`. This prevents many chunks of a single
long note from crowding out other observations. It is not an approximate HNSW
index; large collections need measured latency before scalability claims.

Vectors and queue state are local derived data and are not added to Engram's
sync/export payloads. Imported notes acquire vectors on the receiving machine.
Changing the configured model identity rebuilds the index. Stores whose schema
compatibility is unconfirmed are not extended with semantic tables.

## Agent integrations

Codex and Claude retain their bundled tools, skills and hooks. Register the fork
binary and common data/model settings, and ensure the hook process uses the same
binary and local HTTP server. The `agent` profile still exposes 22 tools in the
reviewed version; semantic retrieval adds none.

The [manual guide's hook matrix](runbooks/manual-local-memory.md#save-reminder-behavior)
documents reminder triggers and limitations. Claude/Codex prompt reminders enter
model context through `additionalContext`; Antigravity uses native `PreInvocation`
injection with a save-age threshold and cooldown. Hook registration remains a
manual step, independent of registering MCP.

Antigravity now has manual configuration fragments under `plugin/antigravity/`
and `engram hook antigravity-session-start`. It uses Antigravity's native
`conversationId`/`workspacePaths` input and `injectSteps` output. It injects a
short protocol, confirmed session identity and bounded project history. Ambiguous
workspaces or unavailable/unconfirmed servers return `{}`. The companion
`engram hook antigravity-pre-invocation` checks save age and injects a brief
reminder with a per-project/session cooldown. The adapter does not implement
passive capture, write-argument binding, closure or compaction hooks.
Its native payload contract is tested; real Antigravity activation remains manual
and has not been verified here.

## Verification on 2026-10-17

Upstream base: `4f4184c`. Go 1.26.2 on macOS arm64; Ollama 0.21.2. The upstream
`modernc.org/sqlite v1.58.0` already bundles sqlite-vec 0.1.9. No Go dependency
upgrade was required.

Behavioral evidence:

- Deterministic store tests cover cross-language candidate plumbing, filters,
  bounded previews, stale work across processes, edits/deletes, fair retries,
  cancellation and future-schema protection.
- Race checks pass for semantic store behavior, CLI diagnostics, Codex prompt
  reminders and native Antigravity startup/reminder adapters.
- Hook regression tests decode model-visible context, overdue-save thresholds,
  debounce, invalid history and fail-open behavior. Claude without `jq` is covered
  independently of the upstream prompt-capture regression.
- Real EmbeddingGemma retrieval passed Spanish-to-English, English-to-Spanish
  and a relevant fact at the end of a long note. Two unrelated queries returned
  no candidates. This small fixture is not a general quality benchmark.
- A real MCP stdio run saved an English payment fix, found it with the Spanish
  query `cobros repetidos`, and fetched the full note by ID. A separate run with
  a deliberately nonexistent model still saved, searched lexically and fetched
  the note. Both exposed the original 22 agent tools.
- Warm MCP search in those individual runs took approximately 140 ms with local
  semantic retrieval and 4 ms with a missing model and lexical fallback. These
  are single measurements, not latency percentiles.
- Manual MCP registration commands for Codex and Claude were exercised against
  disposable account directories; the real account configurations were unchanged.

Run focused checks:

```sh
go test ./internal/store ./cmd/engram -run '^Test(Semantic|Antigravity|MainEmbeddings)' -count=1
go test -race ./internal/store ./cmd/engram -run '^Test(Semantic|Antigravity|MainEmbeddings)' -count=1
go vet ./internal/store ./cmd/engram ./internal/mcp

ENGRAM_TEST_OLLAMA_URL=http://127.0.0.1:11434 \
  go test ./internal/store -run '^TestSemanticSearchLocalOllama$' -count=1 -v
```

The unchanged upstream suite reproduced two failures on this Mac:

- `TestClaudeCodeUserPromptHookWithoutJQPreservesSessionStateAndNudge`: timed out
  waiting for passive prompt capture in the no-jq shell path.
- `TestUpdateInstructions`: macOS implementation returns only the Homebrew
  command while its test also requires the GitHub Releases URL.

Those failures also occur in the clean upstream checkout. They are not reported
as passing. Using `/private/tmp` resolves the separately observed macOS `/var`
alias and Unix socket path-length failures. The broad verification passed all
33 tested packages with only the two reproduced upstream tests excluded:

```sh
TMPDIR=/private/tmp go test ./... \
  -skip '^Test(ClaudeCodeUserPromptHookWithoutJQPreservesSessionStateAndNudge|UpdateInstructions)$'
```

The sqlite-vec auto-extension initializes SQLite during package initialization.
The isolated upstream memory-counter test helper was adapted to shut SQLite down
before changing its process-wide MEMSTATUS configuration; production startup does
not shut SQLite down or change global memory accounting.

Sources: [sqlite-vec bundle](https://pkg.go.dev/modernc.org/sqlite@v1.58.0/vec),
[Ollama embedding API](https://docs.ollama.com/api/embed),
[model variant](https://ollama.com/library/embeddinggemma:300m-qat-q4_0),
[Antigravity hook contract](https://antigravity.google/docs/hooks).
