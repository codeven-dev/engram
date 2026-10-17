package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gentleman-Programming/engram/v3/internal/store"
	versioncheck "github.com/Gentleman-Programming/engram/v3/internal/version"
)

func TestMainEmbeddingsStatusDisabled(t *testing.T) {
	t.Setenv("ENGRAM_EMBED_MODEL", "")
	t.Setenv("ENGRAM_DATA_DIR", t.TempDir())
	previous := storeDefaultConfig
	storeDefaultConfig = store.DefaultConfig
	t.Cleanup(func() { storeDefaultConfig = previous })
	check := checkForUpdates
	checkForUpdates = func(string) versioncheck.CheckResult {
		return versioncheck.CheckResult{Status: versioncheck.StatusUpToDate}
	}
	t.Cleanup(func() { checkForUpdates = check })
	stubExitWithPanic(t)
	withArgs(t, "engram", "embeddings", "status")
	stdout, stderr, exit := captureOutputAndRecover(t, main)
	var status struct {
		Enabled bool `json:"enabled"`
	}
	if exit != nil || stderr != "" || json.Unmarshal([]byte(stdout), &status) != nil || status.Enabled {
		t.Fatalf("status stdout=%q stderr=%q exit=%v", stdout, stderr, exit)
	}
}

func TestMainEmbeddingsIndexBackfillsExistingNotes(t *testing.T) {
	t.Setenv("ENGRAM_EMBED_MODEL", "")
	dir := t.TempDir()
	t.Setenv("ENGRAM_DATA_DIR", dir)
	cfg := store.FallbackConfig(dir)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("cli-index", "payments", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "cli-index", Project: "payments", Title: "Saved before enabling vectors", Content: "Durable note", Type: "learning"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		vectors := make([][]float32, len(req.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer provider.Close()
	t.Setenv("ENGRAM_EMBED_MODEL", "test")
	t.Setenv("ENGRAM_EMBED_URL", provider.URL)
	t.Setenv("ENGRAM_EMBED_DIMENSIONS", "3")
	stubExitWithPanic(t)
	withArgs(t, "engram", "embeddings", "index")
	stdout, stderr, exit := captureOutputAndRecover(t, main)
	var status struct {
		Pending int `json:"pending"`
		Indexed int `json:"indexed_observations"`
	}
	if exit != nil || stderr != "" || json.Unmarshal([]byte(stdout), &status) != nil || status.Pending != 0 || status.Indexed != 1 {
		t.Fatalf("index stdout=%q stderr=%q exit=%v", stdout, stderr, exit)
	}
}
