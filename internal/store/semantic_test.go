package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemanticSearchRecoversAcrossLanguages(t *testing.T) {
	// The embedding service is the external boundary. SQLite and the entire
	// Engram save/search path remain real.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vectors := make([][]float32, len(request.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(provider.Close)
	t.Setenv("ENGRAM_EMBED_MODEL", "test-multilingual")
	t.Setenv("ENGRAM_EMBED_URL", provider.URL)
	t.Setenv("ENGRAM_EMBED_DIMENSIONS", "3")
	s := newTestStore(t)
	t.Logf("semantic configured=%t, active=%t", s.cfg.Semantic != nil, s.semantic != nil)
	if err := s.CreateSession("semantic-session", "payments", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "semantic-session", Project: "payments", Type: "bugfix",
		Title: "Duplicate charges", Content: "Deduplicate payment events inside the transaction using event_id.",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		results, err := s.Search("cobros repetidos", SearchOptions{Project: "payments"})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) == 1 && results[0].ID == id {
			return
		}
		if time.Now().After(deadline) {
			var indexed, pending int
			_ = s.db.QueryRow("SELECT COUNT(*) FROM observation_vectors").Scan(&indexed)
			_ = s.db.QueryRow("SELECT COUNT(*) FROM semantic_pending").Scan(&pending)
			t.Logf("indexed=%d pending=%d", indexed, pending)
			candidates, err := s.semanticCandidates(context.Background(), "cobros repetidos", SearchOptions{Project: "payments"})
			t.Logf("candidates=%v error=%v", candidates, err)
			var storedID int64
			var distance float64
			err = s.db.QueryRow("SELECT observation_id, vec_distance_cosine(embedding,'[1,0,0]') FROM observation_vectors LIMIT 1").Scan(&storedID, &distance)
			t.Logf("vector observation=%d distance=%f err=%v", storedID, distance, err)
			t.Fatalf("cross-language search returned %v; want observation %d", results, id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSemanticProviderRejectsInvalidVectors(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{`},
		{"missing vectors", `{}`},
		{"wrong batch size", `{"embeddings":[]}`},
		{"wrong dimensions", `{"embeddings":[[1,0]]}`},
		{"zero vector", `{"embeddings":[[0,0,0]]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer provider.Close()
			index := semanticIndex{cfg: SemanticConfig{Model: "test", URL: provider.URL, Dimensions: 3}}
			if _, err := index.embed(context.Background(), []string{"note"}); err == nil {
				t.Fatal("invalid provider response accepted")
			}
		})
	}
}

func TestSemanticIndexDoesNotModifyFutureSchema(t *testing.T) {
	t.Setenv("ENGRAM_EMBED_MODEL", "")
	cfg := mustDefaultConfig(t)
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	cfg.Semantic = &SemanticConfig{Model: "test", URL: "http://127.0.0.1:1", Dimensions: 3, MinSimilarity: 0.35}
	future, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer future.Close()
	var count int
	if err := future.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('semantic_meta','semantic_pending','observation_vectors')").Scan(&count); err != nil || count != 0 {
		t.Fatalf("future schema modified: objects=%d error=%v", count, err)
	}
}

func TestSemanticSearchPreservesFiltersAndInvalidatesEdits(t *testing.T) {
	var unavailable atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		vectors := make([][]float32, len(request.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer provider.Close()
	t.Setenv("ENGRAM_EMBED_MODEL", "test")
	t.Setenv("ENGRAM_EMBED_URL", provider.URL)
	t.Setenv("ENGRAM_EMBED_DIMENSIONS", "3")
	s := newTestStore(t)
	s.stopSemanticIndex()
	var target, other int64
	for _, project := range []string{"alpha", "beta"} {
		if err := s.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		for _, scope := range []string{"project", "personal"} {
			for _, kind := range []string{"bugfix", "decision"} {
				id, err := s.AddObservation(AddObservationParams{SessionID: project, Project: project, Scope: scope, Type: kind, Title: project + scope + kind, Content: strings.Repeat("durable observation ", 60)})
				if err != nil {
					t.Fatal(err)
				}
				if project == "alpha" && scope == "project" && kind == "bugfix" {
					target = id
				}
				other = id
			}
		}
	}
	ctx := context.Background()
	if _, err := s.IndexPendingEmbeddings(ctx); err != nil {
		t.Fatal(err)
	}
	opts := SearchOptions{Project: "ALPHA", Scope: "project", Type: "bugfix", Limit: 1}
	full, err := s.Search("recuerdo", opts)
	if err != nil || len(full) != 1 || full[0].ID != target {
		t.Fatalf("filtered full=%v error=%v", full, err)
	}
	preview, err := s.SearchPreviewsContext(ctx, "recuerdo", opts)
	if err != nil || len(preview) != 1 || preview[0].ID != target || !preview[0].Truncated || len([]rune(preview[0].Preview)) > 300 {
		t.Fatalf("filtered preview=%v error=%v", preview, err)
	}
	personal, err := s.Search("recuerdo", SearchOptions{Scope: "personal", Type: "bugfix"})
	if err != nil || len(personal) != 2 {
		t.Fatalf("personal results=%v error=%v", personal, err)
	}
	if err := s.DeleteObservation(other, true); err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	content := "replacement sentinel"
	if _, err := s.UpdateObservation(target, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	// A candidate may have been selected just before the edit. Hydration must
	// recheck eligibility instead of serving its new body with an old vector.
	var hydrated int64
	if err := s.semanticObservationRow(ctx, "id", target, opts).Scan(&hydrated); err != sql.ErrNoRows {
		t.Fatalf("stale candidate remained eligible: %v", err)
	}
	semantic, err := s.Search("recuerdo", opts)
	if err != nil || len(semantic) != 0 {
		t.Fatalf("stale semantic results=%v error=%v", semantic, err)
	}
	lexical, err := s.Search("replacement", opts)
	if err != nil || len(lexical) != 1 || lexical[0].ID != target {
		t.Fatalf("offline lexical=%v error=%v", lexical, err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "alpha", Project: "alpha", Title: "Saved offline", Content: "still durable", Type: "learning"}); err != nil {
		t.Fatalf("offline save failed: %v", err)
	}
	unavailable.Store(false)
	if _, err := s.IndexPendingEmbeddings(ctx); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Search("recuerdo", opts)
	if err != nil || len(updated) != 1 || updated[0].Content != content {
		t.Fatalf("updated semantic=%v error=%v", updated, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SearchContext(canceled, "recuerdo", opts); err != context.Canceled {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestSemanticIndexDoesNotStarveBehindUnembeddableNotes(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, text := range req.Input {
			if strings.Contains(text, "poison") {
				w.WriteHeader(400)
				return
			}
		}
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
	s := newTestStore(t)
	s.stopSemanticIndex()
	if err := s.CreateSession("queue", "payments", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var good int64
	for i := 0; i < 9; i++ {
		content := "poison"
		if i == 8 {
			content = "healthy"
		}
		id, err := s.AddObservation(AddObservationParams{SessionID: "queue", Project: "payments", Title: fmt.Sprint("note ", i), Content: content, Type: "learning"})
		if err != nil {
			t.Fatal(err)
		}
		good = id
	}
	for i := 0; i < 2; i++ {
		_, _ = s.IndexPendingEmbeddings(context.Background())
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM observation_vectors WHERE observation_id=?", good).Scan(&count); err != nil || count == 0 {
		t.Fatalf("healthy note starved: count=%d error=%v", count, err)
	}
}

func TestSemanticIndexRejectsOlderWorkAfterAnotherProcessIndexes(t *testing.T) {
	t.Setenv("ENGRAM_EMBED_MODEL", "test-multilingual")
	t.Setenv("ENGRAM_EMBED_URL", "http://127.0.0.1:1")
	t.Setenv("ENGRAM_EMBED_DIMENSIONS", "3")
	s := newTestStore(t)
	s.stopSemanticIndex()
	if err := s.CreateSession("race", "payments", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "race", Project: "payments", Title: "Original", Content: "old meaning", Type: "learning"})
	if err != nil {
		t.Fatal(err)
	}
	var old pendingEmbedding
	old.id = id
	if err := s.db.QueryRow("SELECT version FROM semantic_pending WHERE observation_id=?", id).Scan(&old.version); err != nil {
		t.Fatal(err)
	}
	// Another process finishes the same version, then a fresh edit arrives.
	other, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.stopSemanticIndex()
	if ok, err := other.publishEmbeddings(context.Background(), old, [][]float32{{1, 0, 0}}); err != nil || !ok {
		t.Fatalf("other publication=%v %v", ok, err)
	}
	content := "new meaning"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.publishEmbeddings(context.Background(), old, [][]float32{{1, 0, 0}}); err != nil || ok {
		t.Fatalf("obsolete publication accepted=%v error=%v", ok, err)
	}
}

func TestSemanticSearchLocalOllama(t *testing.T) {
	endpoint := os.Getenv("ENGRAM_TEST_OLLAMA_URL")
	if endpoint == "" {
		t.Skip("set ENGRAM_TEST_OLLAMA_URL to run real local inference")
	}
	t.Setenv("ENGRAM_EMBED_MODEL", "embeddinggemma:300m-qat-q4_0")
	t.Setenv("ENGRAM_EMBED_URL", endpoint)
	t.Setenv("ENGRAM_EMBED_DIMENSIONS", "768")
	t.Setenv("ENGRAM_EMBED_MIN_SIMILARITY", "")
	s := newTestStore(t)
	if err := s.CreateSession("real-semantic", "payments", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, note := range []struct{ title, content string }{
		{"Duplicate charges", "Deduplicate payment events inside the transaction using event_id."},
		{"Renovación de sesión", "Rotar el token de refresco de forma atómica para evitar sesiones duplicadas."},
		{"Dashboard colors", "Use a blue theme for the dashboard."},
		{"Connection cleanup", strings.Repeat("Routine progress checkpoint. ", 220) + " Unregister event subscribers when closing the websocket to prevent memory leaks from stale listeners retaining buffers."},
	} {
		if _, err := s.AddObservation(AddObservationParams{SessionID: "real-semantic", Project: "payments", Type: "learning", Title: note.title, Content: note.content}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if _, err := s.IndexPendingEmbeddings(ctx); err != nil {
			t.Fatal(err)
		}
		var pending int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM semantic_pending").Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
	}
	for _, tc := range []struct{ query, title string }{
		{"cobros repetidos", "Duplicate charges"},
		{"atomic refresh token rotation", "Renovación de sesión"},
		{"fuga de memoria por listeners al cerrar websocket", "Connection cleanup"},
	} {
		results, err := s.SearchPreviewsContext(ctx, tc.query, SearchOptions{Project: "payments", Limit: 3})
		if err != nil || len(results) == 0 || results[0].Title != tc.title {
			t.Fatalf("query %q: results=%v error=%v", tc.query, results, err)
		}
		t.Logf("query %q: top=%q candidates=%d", tc.query, results[0].Title, len(results))
	}
	for _, query := range []string{"receta de pastel de chocolate", "cultivo de arroz en Ecuador"} {
		results, err := s.SearchPreviewsContext(ctx, query, SearchOptions{Project: "payments"})
		if err != nil || len(results) != 0 {
			t.Fatalf("unrelated query %q returned %v error=%v", query, results, err)
		}
	}
}
