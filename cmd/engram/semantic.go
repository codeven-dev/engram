package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/Gentleman-Programming/engram/v3/internal/store"
)

func cmdEmbeddings(cfg store.Config) error {
	if len(os.Args) != 3 || (os.Args[2] != "status" && os.Args[2] != "index") {
		return errors.New("usage: engram embeddings status|index")
	}
	s, err := store.New(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if os.Args[2] == "index" {
		for {
			status, err := s.EmbeddingStatus(ctx)
			if err != nil {
				return err
			}
			if !status.Enabled {
				return errors.New("local embeddings are disabled; set a valid ENGRAM_EMBED_MODEL configuration")
			}
			if status.Pending == 0 {
				break
			}
			if _, err := s.IndexPendingEmbeddings(ctx); err != nil {
				return errors.New("local embedding indexing unavailable; observations remain saved and searchable lexically")
			}
		}
	}
	status, err := s.EmbeddingStatus(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
