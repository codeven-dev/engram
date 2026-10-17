# Source from the repository root after building ./engram. This changes only
# the current shell environment; it does not install or modify agent settings.
if [ ! -f ./go.mod ] || [ ! -x ./engram ]; then
  printf '%s\n' 'Build ./engram and source scripts/local-memory.sh from the repository root.' >&2
  return 1 2>/dev/null || exit 1
fi

export ENGRAM_REPO="$(pwd -P)"
export ENGRAM_BIN="${ENGRAM_BIN:-$ENGRAM_REPO/engram}"
export PATH="$(dirname "$ENGRAM_BIN"):$PATH"
export ENGRAM_DATA_DIR="${ENGRAM_DATA_DIR:-$HOME/.engram-codeven}"
export ENGRAM_PORT="${ENGRAM_PORT:-7438}"
export ENGRAM_URL="${ENGRAM_URL:-http://127.0.0.1:$ENGRAM_PORT}"
export ENGRAM_EMBED_MODEL="${ENGRAM_EMBED_MODEL:-embeddinggemma:300m-qat-q4_0}"
export ENGRAM_EMBED_URL="${ENGRAM_EMBED_URL:-http://127.0.0.1:11434}"
export ENGRAM_EMBED_DIMENSIONS="${ENGRAM_EMBED_DIMENSIONS:-768}"
export ENGRAM_EMBED_MIN_SIMILARITY="${ENGRAM_EMBED_MIN_SIMILARITY:-0.35}"
