# Runbook: Manual local memory setup for three agents

Build the fork, enable its optional local embeddings, then register the same
binary and data directory with Codex Qbit, Claude Code and Antigravity. No
installer or configuration changes are performed by this guide until you run its
commands. There is no remote server deployment.

## 1. Inspect prerequisites

From the fork's root:

```sh
go version
ollama --version
command -v jq
command -v curl
git remote -v
```

Go 1.25.10 or later builds Engram. Ollama 0.11.10 or later supports the selected
EmbeddingGemma model. `jq` and `curl` support the existing Codex/Claude shell
hooks. SQLite and sqlite-vec are compiled into the Go binary; no SQLite server,
Python environment or vector database service is needed.

## 2. Build and configure the current shell

```sh
go build -ldflags '-X main.version=dev-codeven-vectors' -o engram ./cmd/engram
source scripts/local-memory.sh
"$ENGRAM_BIN" version
```

The sourced file changes only this shell. Repeat these two commands from the
repository root in each terminal used to launch an agent or the HTTP server:

```sh
source scripts/local-memory.sh
printf '%s\n' "$ENGRAM_BIN" "$ENGRAM_DATA_DIR" "$ENGRAM_URL" "$ENGRAM_EMBED_URL"
```

While evaluating this fork, rebuild this checkout to update it. Upstream
Homebrew, release downloads and `go install` commands install the original
Engram binary and do not include this branch's extension.

Defaults:

| Setting | Value |
| --- | --- |
| Binary | This checkout's absolute `engram` path |
| Memory | `~/.engram-codeven/engram.db` |
| Local Engram HTTP | `http://127.0.0.1:7438` |
| Local Ollama HTTP | `http://127.0.0.1:11434` |
| Model | `embeddinggemma:300m-qat-q4_0` |
| Vector dimensions | 768 |
| Minimum cosine similarity | 0.35, an initial experimental value |

All three agents must use the same model settings and memory directory. Project
identity remains Engram's existing resolver; do not set a global `ENGRAM_PROJECT`
for unrelated repositories. If a repository needs an explicit identity, its
`.engram/config.json` uses `{"project_name":"payments"}`. Worktrees and existing
Git bindings retain Engram's normal behavior.

The model tag is explicit: the default unquantized `embeddinggemma:latest` is
larger. The selected quantized download is approximately 239 MB. Inference also
needs runtime memory beyond the downloaded weights.

## 3. Start the optional local model and Engram server

In a separate terminal, start Ollama if it is not already running:

```sh
ollama serve
```

Download once:

```sh
ollama pull embeddinggemma:300m-qat-q4_0
```

In another terminal, source the environment and start the shared local API:

```sh
source scripts/local-memory.sh
"$ENGRAM_BIN" serve "$ENGRAM_PORT"
```

Keep this terminal running. MCP remains stdio; the HTTP API supplies host session
and context operations to hooks. The explicit `ENGRAM_URL` tells the original
plugins to use this manually managed server.

Inspect and process the index from another configured terminal:

```sh
"$ENGRAM_BIN" embeddings status
"$ENGRAM_BIN" embeddings index
curl --fail --silent "$ENGRAM_URL/health"
```

`status` describes configured index coverage, not model health. `index` runs
pending work for up to five minutes and reports an error if inference cannot
complete. That diagnostic error does not mean a note was lost. MCP/HTTP stores
also process bounded batches in the background. A short CLI save may exit before
its embedding is ready; its pending work is durable.

## 4. Codex Qbit

Register the MCP server only in the selected account:

```sh
CODEX_HOME="$HOME/.codex-cuenta-actual" codex mcp add engram \
  --env "ENGRAM_DATA_DIR=$ENGRAM_DATA_DIR" \
  --env "ENGRAM_EMBED_MODEL=$ENGRAM_EMBED_MODEL" \
  --env "ENGRAM_EMBED_URL=$ENGRAM_EMBED_URL" \
  --env "ENGRAM_EMBED_DIMENSIONS=$ENGRAM_EMBED_DIMENSIONS" \
  --env "ENGRAM_EMBED_MIN_SIMILARITY=$ENGRAM_EMBED_MIN_SIMILARITY" \
  -- "$ENGRAM_BIN" mcp --tools=agent

CODEX_HOME="$HOME/.codex-cuenta-actual" codex plugin marketplace add "$ENGRAM_REPO"
CODEX_HOME="$HOME/.codex-cuenta-actual" codex plugin add engram@engram
CODEX_HOME="$HOME/.codex-cuenta-actual" codex mcp list
```

Launch Codex from this configured shell, inside the project you want to work on:

```sh
CODEX_HOME="$HOME/.codex-cuenta-actual" codex
```

Review and trust the plugin hooks through `/hooks`. This is Codex's normal trust
step; it is not bypassed. Start a new session after configuration changes.
`~/.codex/config.toml`, the personal account, is outside these commands.

Do not set `model_instructions_file` or `experimental_compact_prompt_file` to an
Engram file. The existing plugin injects its protocol additively. Do not edit or
replace global `AGENTS.md` symlinks.

## 5. Claude Code

Install the plugin assets from this local fork and register the binary manually:

```sh
claude plugin marketplace add "$ENGRAM_REPO"
claude plugin install engram@engram
claude mcp add engram --scope user --transport stdio \
  --env "ENGRAM_DATA_DIR=$ENGRAM_DATA_DIR" \
  --env "ENGRAM_EMBED_MODEL=$ENGRAM_EMBED_MODEL" \
  --env "ENGRAM_EMBED_URL=$ENGRAM_EMBED_URL" \
  --env "ENGRAM_EMBED_DIMENSIONS=$ENGRAM_EMBED_DIMENSIONS" \
  --env "ENGRAM_EMBED_MIN_SIMILARITY=$ENGRAM_EMBED_MIN_SIMILARITY" \
  -- "$ENGRAM_BIN" mcp --tools=agent
claude mcp list
```

If `CLAUDE_CONFIG_DIR` selects a separate account, retain that variable for both
registration and launch. Start Claude from the configured shell and the desired
project. Its existing hooks own session startup, context and compaction recovery;
its bundled memory skill describes writing and retrieval. No new skill or global
`CLAUDE.md` edit is required for semantic search.

If a marketplace called `engram` already exists, inspect its source before
replacing it. Registering the fork must not silently retain an upstream cache.

## 6. Antigravity

Upstream registers MCP and writes a global protocol for `antigravity-cli`, but
does not ship an Antigravity lifecycle hook adapter. This fork adds a native
`SessionStart` adapter with camelCase input and `injectSteps` output. It registers
the server-confirmed runtime identity and injects a short memory protocol plus
bounded project history. It does not reuse Claude/Codex hook envelopes.

Generate configuration fragments with absolute paths, without editing your
existing agent settings:

```sh
python3 - <<'PY'
import json, os, pathlib, shlex
out = pathlib.Path('/tmp/engram-manual-config')
out.mkdir(exist_ok=True)
keys = ['ENGRAM_DATA_DIR', 'ENGRAM_EMBED_MODEL', 'ENGRAM_EMBED_URL',
        'ENGRAM_EMBED_DIMENSIONS', 'ENGRAM_EMBED_MIN_SIMILARITY']
entry = {'command': os.environ['ENGRAM_BIN'], 'args': ['mcp', '--tools=agent'],
         'env': {key: os.environ[key] for key in keys}}
command = shlex.join(['env', 'ENGRAM_URL=' + os.environ['ENGRAM_URL'],
                     os.environ['ENGRAM_BIN'], 'hook', 'antigravity-session-start'])
hook = {'engram': {'SessionStart': [{'type': 'command', 'command': command, 'timeout': 3}]}}
(out/'mcp_config.json').write_text(json.dumps({'mcpServers': {'engram': entry}}, indent=2)+'\n')
(out/'hooks.json').write_text(json.dumps(hook, indent=2)+'\n')
print(out)
PY
```

Merge `mcpServers.engram` from the generated fragment into
`~/.gemini/config/mcp_config.json`. Merge the generated top-level `engram` hook
into `~/.gemini/config/hooks.json`, preserving other MCP servers and hooks.
For a project-only hook, the documented alternative is `.agents/hooks.json`.
Choose one hook location to avoid duplicate startup injection.

Review the hook in Antigravity's Customizations/ Hooks interface and restart the
agent. The shared HTTP server must already be running. Launching the IDE from a
desktop icon does not inherit terminal exports; the generated fragments therefore
contain absolute paths and explicit environment settings.

No `GEMINI.md` modification is necessary with this startup adapter. Multiple
mounted workspaces, unavailable servers or unconfirmed registrations produce an
empty hook response rather than guessing a project or session ID. This initial
adapter does not implement periodic save nudges, passive capture, session closure
or compaction hooks. `Stop` is not treated as conversation termination.

The payload contract is covered by automated adapter tests. An installed
Antigravity runtime has not yet been exercised in this workspace; verify the
hook and tools in a new real session after your manual installation.

## 7. Verify retrieval and model failure

In a new agent session inside a test project, ask the agent to save an English
note describing a solved bug. Wait for `embeddings status` to show no pending
work, then describe the same symptom in Spanish. Inspect `mem_search` candidates
and `mem_get_observation` before treating the note as applicable to current code.

Stop Ollama, then save another note and search using its literal words. Both
operations must still succeed through SQLite and FTS. Restart Ollama and run
`embeddings index` to process pending notes. Search is still initiated by the
agent; the vector extension does not add per-message automatic retrieval.

## 8. Disable or rebuild the derived index

Unset the model variable in the HTTP server and each MCP registration, then
restart those processes:

```sh
unset ENGRAM_EMBED_MODEL
"$ENGRAM_BIN" embeddings status
```

Do not source `scripts/local-memory.sh` again when testing disabled mode: that
file explicitly opts into its model default. Do not delete observations to fix a
vector index. Changing the configured model, dimensions or endpoint automatically
rebuilds derived vectors. If weights change behind the same model tag, stop all
Engram processes, delete only the row in `semantic_meta`, and restart with the
same model configuration to request a complete rebuild.

For validation results and known upstream failures, see
[local semantic retrieval](../LOCAL-SEMANTIC-SEARCH.md).

Sources: [Ollama embedding API](https://docs.ollama.com/api/embed),
[quantized model](https://ollama.com/library/embeddinggemma:300m-qat-q4_0),
[Codex MCP](https://developers.openai.com/codex/mcp),
[Claude Code MCP](https://code.claude.com/docs/en/mcp),
[Antigravity hooks](https://antigravity.google/docs/hooks),
[Antigravity MCP](https://antigravity.google/docs/mcp).
