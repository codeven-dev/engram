package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite/vec"
)

// SemanticConfig enables optional local inference. A nil configuration keeps
// the original lexical search. Runtime requests never download model weights.
type SemanticConfig struct {
	Model         string
	URL           string
	Dimensions    int
	MinSimilarity float64
	Timeout       time.Duration
}

type semanticIndex struct {
	cfg       SemanticConfig
	key       string
	cancel    context.CancelFunc
	done      chan struct{}
	indexing  chan struct{}
	mu        sync.Mutex
	lastError string
}

// EmbeddingStatus reports local index coverage, not model availability. No
// inference or network request is needed to inspect it.
type EmbeddingStatus struct {
	Enabled             bool   `json:"enabled"`
	Model               string `json:"model,omitempty"`
	Pending             int    `json:"pending"`
	IndexedObservations int    `json:"indexed_observations"`
	IndexedChunks       int    `json:"indexed_chunks"`
	LastError           string `json:"last_error,omitempty"`
}

func (s *Store) EmbeddingStatus(ctx context.Context) (EmbeddingStatus, error) {
	status := EmbeddingStatus{Enabled: s.semantic != nil}
	if s.cfg.Semantic != nil {
		status.Model = s.cfg.Semantic.Model
	}
	if s.semantic == nil {
		if s.cfg.Semantic != nil {
			status.LastError = "configuration or index initialization unavailable"
		}
		return status, nil
	}
	s.semantic.mu.Lock()
	status.LastError = s.semantic.lastError
	s.semantic.mu.Unlock()
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM semantic_pending").Scan(&status.Pending); err != nil {
		return status, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT v.observation_id),COUNT(*) FROM observation_vectors v
	 JOIN observations o ON o.id=v.observation_id WHERE o.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM semantic_pending p WHERE p.observation_id=o.id)`).Scan(&status.IndexedObservations, &status.IndexedChunks)
	return status, err
}

func semanticConfigFromEnv() *SemanticConfig {
	model := strings.TrimSpace(os.Getenv("ENGRAM_EMBED_MODEL"))
	if model == "" {
		return nil
	}
	cfg := &SemanticConfig{Model: model, URL: "http://127.0.0.1:11434", Dimensions: 768, MinSimilarity: 0.35, Timeout: 2 * time.Second}
	if value := strings.TrimSpace(os.Getenv("ENGRAM_EMBED_URL")); value != "" {
		cfg.URL = value
	}
	if value := strings.TrimSpace(os.Getenv("ENGRAM_EMBED_DIMENSIONS")); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			n = 0
		}
		cfg.Dimensions = n
	}
	if value := strings.TrimSpace(os.Getenv("ENGRAM_EMBED_MIN_SIMILARITY")); value != "" {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			n = math.NaN()
		}
		cfg.MinSimilarity = n
	}
	return cfg
}

func (s *Store) startSemanticIndex() {
	if s.cfg.Semantic == nil {
		return
	}
	if !s.compatibilityReady {
		log.Print("[engram] semantic retrieval disabled: database compatibility is not confirmed")
		return
	}
	cfg := *s.cfg.Semantic
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || cfg.Model == "" || cfg.Dimensions < 1 || cfg.Dimensions > 4096 || math.IsNaN(cfg.MinSimilarity) || cfg.MinSimilarity < 0 || cfg.MinSimilarity > 1 {
		log.Print("[engram] semantic retrieval disabled: invalid local embedding configuration")
		return
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	key := fmt.Sprintf("%s|%s|%d|chunk512-search-v1", cfg.URL, cfg.Model, cfg.Dimensions)
	if err := s.prepareSemanticIndex(key, cfg.Dimensions); err != nil {
		log.Print("[engram] semantic retrieval disabled: index initialization failed")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.semantic = &semanticIndex{cfg: cfg, key: key, cancel: cancel, done: make(chan struct{}), indexing: make(chan struct{}, 1)}
	go func() {
		defer close(s.semantic.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			pass, stop := context.WithTimeout(ctx, 30*time.Second)
			_, err := s.IndexPendingEmbeddings(pass)
			stop()
			if err != nil && ctx.Err() == nil {
				s.semanticError("background indexing unavailable")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Store) stopSemanticIndex() {
	if s.semantic == nil {
		return
	}
	s.semantic.cancel()
	<-s.semantic.done
}

func (s *Store) semanticError(reason string) {
	s.semantic.mu.Lock()
	defer s.semantic.mu.Unlock()
	if s.semantic.lastError != reason && reason != "" {
		log.Printf("[engram] semantic retrieval degraded: %s; lexical search remains available", reason)
	}
	s.semantic.lastError = reason
}

func (s *Store) prepareSemanticIndex(key string, dimensions int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
	CREATE TABLE IF NOT EXISTS semantic_meta (id INTEGER PRIMARY KEY CHECK(id=1), model_key TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS semantic_pending (observation_id INTEGER PRIMARY KEY, version TEXT NOT NULL, attempted_at INTEGER NOT NULL DEFAULT 0);
	CREATE TRIGGER IF NOT EXISTS semantic_insert AFTER INSERT ON observations BEGIN
	 INSERT INTO semantic_pending(observation_id,version) VALUES(new.id,hex(randomblob(16))) ON CONFLICT(observation_id) DO UPDATE SET version=excluded.version,attempted_at=0;
	END;
	CREATE TRIGGER IF NOT EXISTS semantic_update AFTER UPDATE OF title,content,project,scope,type,deleted_at ON observations
	WHEN old.title IS NOT new.title OR old.content IS NOT new.content OR old.project IS NOT new.project OR old.scope IS NOT new.scope OR old.type IS NOT new.type OR old.deleted_at IS NOT new.deleted_at BEGIN
	 INSERT INTO semantic_pending(observation_id,version) VALUES(new.id,hex(randomblob(16))) ON CONFLICT(observation_id) DO UPDATE SET version=excluded.version,attempted_at=0;
	END;
	CREATE TRIGGER IF NOT EXISTS semantic_delete AFTER DELETE ON observations BEGIN
	 INSERT INTO semantic_pending(observation_id,version) VALUES(old.id,hex(randomblob(16))) ON CONFLICT(observation_id) DO UPDATE SET version=excluded.version,attempted_at=0;
	END;`)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRow("SELECT model_key FROM semantic_meta WHERE id=1").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != key {
		if _, err = tx.Exec("DROP TABLE IF EXISTS observation_vectors"); err != nil {
			return err
		}
		if _, err = tx.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE observation_vectors USING vec0(observation_id INTEGER, embedding FLOAT[%d] distance_metric=cosine)`, dimensions)); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO semantic_meta VALUES(1,?) ON CONFLICT(id) DO UPDATE SET model_key=excluded.model_key`, key); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO semantic_pending(observation_id,version) SELECT id,hex(randomblob(16)) FROM observations WHERE true ON CONFLICT(observation_id) DO UPDATE SET version=excluded.version,attempted_at=0`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type pendingEmbedding struct {
	id             int64
	version        string
	title, content string
	live           bool
}

// IndexPendingEmbeddings processes at most eight observations. Inference runs
// outside SQL transactions; queue versions prevent stale publication.
func (s *Store) IndexPendingEmbeddings(ctx context.Context) (int, error) {
	if s.semantic == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case s.semantic.indexing <- struct{}{}:
		defer func() { <-s.semantic.indexing }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.observation_id,p.version,COALESCE(o.title,''),COALESCE(o.content,''),o.id IS NOT NULL AND o.deleted_at IS NULL
	 FROM semantic_pending p LEFT JOIN observations o ON o.id=p.observation_id ORDER BY p.attempted_at,p.observation_id LIMIT 8`)
	if err != nil {
		return 0, err
	}
	var pending []pendingEmbedding
	for rows.Next() {
		var p pendingEmbedding
		if err = rows.Scan(&p.id, &p.version, &p.title, &p.content, &p.live); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	indexed := 0
	var failures error
	for _, p := range pending {
		if _, err = s.db.ExecContext(ctx, "UPDATE semantic_pending SET attempted_at=? WHERE observation_id=? AND version=?", time.Now().UnixNano(), p.id, p.version); err != nil {
			return indexed, err
		}
		var vectors [][]float32
		if p.live {
			text := []rune(p.title + "\n" + p.content)
			var chunks []string
			for start := 0; start < len(text); start += 448 {
				end := min(start+512, len(text))
				chunks = append(chunks, "title: none | text: "+string(text[start:end]))
				if end == len(text) {
					break
				}
			}
			vectors, err = s.semantic.embed(ctx, chunks)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
		}
		published, err := s.publishEmbeddings(ctx, p, vectors)
		if err != nil {
			return indexed, err
		}
		if published {
			indexed++
		}
	}
	if failures == nil {
		s.semanticError("")
	}
	return indexed, failures
}

func (s *Store) publishEmbeddings(ctx context.Context, p pendingEmbedding, vectors [][]float32) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var version string
	var key string
	err = tx.QueryRowContext(ctx, "SELECT version FROM semantic_pending WHERE observation_id=?", p.id).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT model_key FROM semantic_meta WHERE id=1").Scan(&key); err != nil {
		return false, err
	}
	if version != p.version || key != s.semantic.key {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM observation_vectors WHERE observation_id=?", p.id); err != nil {
		return false, err
	}
	for _, vector := range vectors {
		encoded, _ := json.Marshal(vector)
		if _, err = tx.ExecContext(ctx, "INSERT INTO observation_vectors(observation_id,embedding) VALUES(?,?)", p.id, string(encoded)); err != nil {
			return false, err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM semantic_pending WHERE observation_id=? AND version=?", p.id, p.version); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (index *semanticIndex) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"model": index.cfg.Model, "input": texts, "truncate": false, "keep_alive": "10m"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(index.cfg.URL, "/")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding HTTP status %d", response.StatusCode)
	}
	var result struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embeddings) != len(texts) {
		return nil, errors.New("embedding batch size mismatch")
	}
	for _, vector := range result.Embeddings {
		if len(vector) != index.cfg.Dimensions {
			return nil, errors.New("embedding dimensions mismatch")
		}
		norm := 0.0
		for _, v := range vector {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, errors.New("invalid embedding value")
			}
			norm += float64(v) * float64(v)
		}
		if norm == 0 {
			return nil, errors.New("zero embedding")
		}
	}
	return result.Embeddings, nil
}

type semanticCandidate struct {
	id       int64
	distance float64
}

func (s *Store) semanticCandidates(ctx context.Context, query string, opts SearchOptions) ([]semanticCandidate, error) {
	if s.semantic == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.semantic.cfg.Timeout)
	defer cancel()
	prefix := ""
	if strings.HasPrefix(s.semantic.cfg.Model, "embeddinggemma") {
		prefix = "task: search result | query: "
	}
	vectors, err := s.semantic.embed(ctx, []string{prefix + query})
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(vectors[0])
	// vec0 is an exact flat index. Aggregate chunk distances before LIMIT so
	// many chunks of one observation cannot crowd other observations out.
	q := `SELECT v.observation_id, MIN(vec_distance_cosine(v.embedding,?)) AS best_distance
	 FROM observation_vectors v JOIN observations o ON o.id=v.observation_id
	 WHERE o.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM semantic_pending p WHERE p.observation_id=o.id)
	 AND (SELECT model_key FROM semantic_meta WHERE id=1)=?`
	args := []any{string(encoded), s.semantic.key}
	if opts.Project != "" {
		project, _ := NormalizeProject(opts.Project)
		q += " AND LOWER(o.project)=?"
		args = append(args, project)
	}
	if opts.Scope != "" {
		q += " AND o.scope=?"
		args = append(args, normalizeScope(opts.Scope))
	}
	if opts.Type != "" {
		q += " AND o.type=?"
		args = append(args, opts.Type)
	}
	q += " GROUP BY v.observation_id HAVING best_distance<=? ORDER BY best_distance, v.observation_id LIMIT ?"
	args = append(args, 1-s.semantic.cfg.MinSimilarity, s.searchLimit(opts.Limit))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []semanticCandidate
	for rows.Next() {
		var candidate semanticCandidate
		if err = rows.Scan(&candidate.id, &candidate.distance); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func (s *Store) searchLimit(limit int) int {
	if limit <= 0 {
		limit = 10
	}
	return min(limit, s.cfg.MaxSearchResults)
}

func (s *Store) semanticObservationRow(ctx context.Context, columns string, id int64, opts SearchOptions) *sql.Row {
	q := "SELECT " + columns + ` FROM observations o WHERE o.id=? AND o.deleted_at IS NULL
	 AND NOT EXISTS(SELECT 1 FROM semantic_pending p WHERE p.observation_id=o.id)
	 AND (SELECT model_key FROM semantic_meta WHERE id=1)=?`
	args := []any{id, s.semantic.key}
	if opts.Project != "" {
		project, _ := NormalizeProject(opts.Project)
		q += " AND LOWER(o.project)=?"
		args = append(args, project)
	}
	if opts.Scope != "" {
		q += " AND o.scope=?"
		args = append(args, normalizeScope(opts.Scope))
	}
	if opts.Type != "" {
		q += " AND o.type=?"
		args = append(args, opts.Type)
	}
	return s.db.QueryRowContext(ctx, q, args...)
}

// SearchContext adds optional semantic candidates without making inference a
// prerequisite for lexical retrieval or weakening caller cancellation.
func (s *Store) SearchContext(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	lexical, err := s.searchLexicalContext(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	candidates, err := s.semanticCandidates(ctx, query, opts)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		s.semanticError("embedding search unavailable")
		return lexical, nil
	}
	if len(candidates) == 0 {
		return lexical, nil
	}
	var semantic []SearchResult
	for _, candidate := range candidates {
		var r SearchResult
		err = scanObservationRow(s.semanticObservationRow(ctx, observationSelectColumns, candidate.id, opts), &r.Observation)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return lexical, nil
		}
		r.Rank = candidate.distance
		semantic = append(semantic, r)
	}
	return fuseSemantic(lexical, semantic, s.searchLimit(opts.Limit), query, func(r SearchResult) (int64, *string) { return r.ID, r.TopicKey }), nil
}

// SearchPreviewsContext uses the same fusion policy while hydrating only bounded
// previews for MCP and HTTP callers.
func (s *Store) SearchPreviewsContext(ctx context.Context, query string, opts SearchOptions) ([]SearchPreviewResult, error) {
	lexical, err := s.searchLexicalPreviewsContext(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	candidates, err := s.semanticCandidates(ctx, query, opts)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		s.semanticError("embedding search unavailable")
		return lexical, nil
	}
	if len(candidates) == 0 {
		return lexical, nil
	}
	var semantic []SearchPreviewResult
	for _, candidate := range candidates {
		var r SearchPreviewResult
		err = scanSearchPreviewRow(s.semanticObservationRow(ctx, `id,ifnull(sync_id,''),type,title,substr(content,1,300),length(content)>300,project,topic_key,scope,review_after,pinned,created_at`, candidate.id, opts), &r, false)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return lexical, nil
		}
		r.Rank = candidate.distance
		semantic = append(semantic, r)
	}
	return fuseSemantic(lexical, semantic, s.searchLimit(opts.Limit), query, func(r SearchPreviewResult) (int64, *string) { return r.ID, r.TopicKey }), nil
}

func fuseSemantic[T any](lexical, semantic []T, limit int, query string, identity func(T) (int64, *string)) []T {
	type ranked struct {
		item  T
		score float64
		exact bool
		order int
	}
	items := map[int64]*ranked{}
	for _, list := range [][]T{lexical, semantic} {
		for position, item := range list {
			id, topic := identity(item)
			entry, ok := items[id]
			if !ok {
				entry = &ranked{item: item, order: len(items), exact: topic != nil && *topic == query}
				items[id] = entry
			}
			entry.score += 1.0 / float64(60+position+1)
		}
	}
	ordered := make([]*ranked, 0, len(items))
	for _, entry := range items {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.exact != b.exact {
			return a.exact
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.order < b.order
	})
	result := make([]T, 0, min(limit, len(ordered)))
	for _, entry := range ordered[:min(limit, len(ordered))] {
		result = append(result, entry.item)
	}
	return result
}
