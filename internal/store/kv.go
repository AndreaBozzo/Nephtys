// Package store provides persistent storage for stream configurations
// using NATS JetStream KV buckets.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"nephtys/internal/domain"
)

const bucketName = "nephtys_streams"

// StreamStore persists StreamSourceConfig entries in a JetStream KV bucket.
type StreamStore struct {
	kv     nats.KeyValue
	logger *slog.Logger
}

// NewStreamStore creates or opens the KV bucket for stream configurations.
func NewStreamStore(js nats.JetStreamContext) (*StreamStore, error) {
	kv, err := js.KeyValue(bucketName)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:      bucketName,
			Description: "Nephtys stream source configurations",
			Storage:     nats.FileStorage,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("kv store init: %w", err)
	}

	slog.Info("Stream config store ready", "bucket", bucketName)
	return &StreamStore{kv: kv, logger: slog.With("component", "store")}, nil
}

// Put persists a stream source configuration.
func (s *StreamStore) Put(cfg domain.StreamSourceConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	_, err = s.kv.Put(cfg.ID, data)
	if err != nil {
		return fmt.Errorf("kv put %q: %w", cfg.ID, err)
	}
	s.logger.Info("Stream config saved", "id", cfg.ID)
	return nil
}

// Delete removes a stream source configuration by ID.
func (s *StreamStore) Delete(id string) error {
	if err := s.kv.Delete(id); err != nil {
		return fmt.Errorf("kv delete %q: %w", id, err)
	}
	s.logger.Info("Stream config deleted", "id", id)
	return nil
}

// Get retrieves a single stream source configuration by ID.
func (s *StreamStore) Get(id string) (domain.StreamSourceConfig, error) {
	entry, err := s.kv.Get(id)
	if err != nil {
		return domain.StreamSourceConfig{}, fmt.Errorf("kv get %q: %w", id, err)
	}
	var cfg domain.StreamSourceConfig
	if err := json.Unmarshal(entry.Value(), &cfg); err != nil {
		return domain.StreamSourceConfig{}, fmt.Errorf("unmarshal config %q: %w", id, err)
	}
	return cfg, nil
}

// List returns all persisted stream source configurations.
func (s *StreamStore) List() ([]domain.StreamSourceConfig, error) {
	keys, err := s.kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil, nil // empty store
	}
	if err != nil {
		return nil, fmt.Errorf("kv keys: %w", err)
	}

	configs := make([]domain.StreamSourceConfig, 0, len(keys))
	for _, key := range keys {
		entry, err := s.kv.Get(key)
		if err != nil {
			s.logger.Warn("Skipping unreadable key", "key", key, "error", err)
			continue
		}
		var cfg domain.StreamSourceConfig
		if err := json.Unmarshal(entry.Value(), &cfg); err != nil {
			s.logger.Warn("Skipping malformed config", "key", key, "error", err)
			continue
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}
