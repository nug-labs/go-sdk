package nuglabs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const defaultSyncPollInterval = time.Minute

// ClientOptions configures the high-level Go client.
type ClientOptions struct {
	// SyncInterval controls how often the background loop asks WASM for due sync actions.
	SyncInterval time.Duration
	// HTTPClient allows custom transport behavior for sync calls.
	HTTPClient *http.Client
	// DatasetURL overrides the default dataset endpoint.
	DatasetURL string
	// RulesURL overrides the default rules endpoint.
	RulesURL string
	// AutoSync controls whether the background tick/sync loop starts automatically.
	AutoSync bool
	// StorageDir enables disk persistence for dataset, rules, and ETags.
	// When empty, the client is memory-only.
	StorageDir string
}

// ArtifactSyncResult represents sync state for one artifact.
type ArtifactSyncResult struct {
	Changed bool
	Status  string
	ETag    string
}

// SyncResult includes dataset + rules sync outcomes.
type SyncResult struct {
	Dataset ArtifactSyncResult
	Rules   ArtifactSyncResult
}

// Client is the high-level local-first SDK surface for Go applications.
type Client struct {
	engine *Engine

	httpClient   *http.Client
	syncInterval time.Duration
	datasetURL   string
	rulesURL     string
	storageDir   string

	mu          sync.Mutex
	datasetETag string
	rulesETag   string

	done chan struct{}
	wg   sync.WaitGroup
}

// NewClient creates a client, loads bundled rules+dataset into WASM, and (by default) starts background sync checks.
func NewClient(ctx context.Context, opts *ClientOptions) (*Client, error) {
	engine, err := Load(ctx)
	if err != nil {
		return nil, err
	}

	if err := engine.LoadBundledRules(ctx); err != nil {
		_ = engine.Close(ctx)
		return nil, err
	}
	if err := engine.LoadBundledDataset(ctx); err != nil {
		_ = engine.Close(ctx)
		return nil, err
	}

	c := &Client{
		engine:       engine,
		httpClient:   http.DefaultClient,
		syncInterval: defaultSyncPollInterval,
		datasetURL:   StrainsDatasetURL,
		rulesURL:     RulesURL,
		storageDir:   "",
		done:         make(chan struct{}),
		datasetETag:  "",
		rulesETag:    "",
	}

	autoSync := true
	if opts != nil {
		if opts.HTTPClient != nil {
			c.httpClient = opts.HTTPClient
		}
		if opts.SyncInterval > 0 {
			c.syncInterval = opts.SyncInterval
		}
		if opts.DatasetURL != "" {
			c.datasetURL = opts.DatasetURL
		}
		if opts.RulesURL != "" {
			c.rulesURL = opts.RulesURL
		}
		if opts.StorageDir != "" {
			c.storageDir = opts.StorageDir
		}
		autoSync = opts.AutoSync
	}
	if err := c.loadPersistedState(ctx); err != nil {
		_ = engine.Close(ctx)
		return nil, err
	}

	if autoSync {
		c.startBackgroundSync()
	}
	return c, nil
}

// Close stops background workers and closes the underlying WASM engine.
func (c *Client) Close(ctx context.Context) error {
	close(c.done)
	c.wg.Wait()
	return c.engine.Close(ctx)
}

// GetStrain returns a strain object or nil when not found.
func (c *Client) GetStrain(ctx context.Context, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.engine.GetStrain(ctx, name)
}

// GetAllStrains returns all strains currently loaded in memory.
func (c *Client) GetAllStrains(ctx context.Context) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.engine.GetAllStrains(ctx)
}

// SearchStrains performs normalized partial search over names and aliases.
func (c *Client) SearchStrains(ctx context.Context, query string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.engine.SearchStrains(ctx, query)
}

// ForceResync runs dataset + rules sync.
func (c *Client) ForceResync(ctx context.Context) (SyncResult, error) {
	ds, err := c.ForceResyncDataset(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	rl, err := c.ForceResyncRules(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Dataset: ds, Rules: rl}, nil
}

// ForceResyncDataset fetches dataset with If-None-Match behavior and updates WASM when changed.
func (c *Client) ForceResyncDataset(ctx context.Context) (ArtifactSyncResult, error) {
	return c.forceResyncArtifact(ctx, "dataset")
}

// ForceResyncRules fetches rules with If-None-Match behavior and updates WASM when changed.
// A 404 response is treated as not-modified for backward-compatible deployments.
func (c *Client) ForceResyncRules(ctx context.Context) (ArtifactSyncResult, error) {
	return c.forceResyncArtifact(ctx, "rules")
}

func (c *Client) startBackgroundSync() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.syncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case now := <-ticker.C:
				_ = c.tickOnce(context.Background(), now)
			}
		}
	}()
}

func (c *Client) tickOnce(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	actions, err := c.engine.TickActions(ctx, now.UnixMilli())
	c.mu.Unlock()
	if err != nil {
		return err
	}
	for _, action := range actions {
		switch action {
		case "dataset":
			if _, err := c.ForceResyncDataset(ctx); err != nil {
				return err
			}
		case "rules":
			if _, err := c.ForceResyncRules(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) forceResyncArtifact(ctx context.Context, artifact string) (ArtifactSyncResult, error) {
	c.mu.Lock()
	var url, etag string
	switch artifact {
	case "dataset":
		url = c.datasetURL
		etag = c.datasetETag
	case "rules":
		url = c.rulesURL
		etag = c.rulesETag
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ArtifactSyncResult{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return ArtifactSyncResult{}, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotModified || (artifact == "rules" && res.StatusCode == http.StatusNotFound) {
		return ArtifactSyncResult{
			Changed: false,
			Status:  "not-modified",
			ETag:    etag,
		}, nil
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return ArtifactSyncResult{}, io.ErrUnexpectedEOF
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return ArtifactSyncResult{}, err
	}
	newETag := res.Header.Get("ETag")

	c.mu.Lock()
	defer c.mu.Unlock()
	switch artifact {
	case "dataset":
		if err := c.engine.LoadDatasetJSON(ctx, string(body)); err != nil {
			return ArtifactSyncResult{}, err
		}
		c.datasetETag = newETag
		if err := c.persistArtifact("dataset", string(body)); err != nil {
			return ArtifactSyncResult{}, err
		}
	case "rules":
		if err := c.engine.LoadRulesJSON(ctx, string(body)); err != nil {
			return ArtifactSyncResult{}, err
		}
		c.rulesETag = newETag
		if err := c.persistArtifact("rules", string(body)); err != nil {
			return ArtifactSyncResult{}, err
		}
	default:
		return ArtifactSyncResult{}, fmt.Errorf("unknown artifact: %s", artifact)
	}
	if err := c.persistETags(); err != nil {
		return ArtifactSyncResult{}, err
	}
	return ArtifactSyncResult{
		Changed: true,
		Status:  "updated",
		ETag:    newETag,
	}, nil
}

func (c *Client) loadPersistedState(ctx context.Context) error {
	if c.storageDir == "" {
		return nil
	}
	if err := os.MkdirAll(c.storageDir, 0o755); err != nil {
		return err
	}

	if raw, err := os.ReadFile(filepath.Join(c.storageDir, "dataset.etag")); err == nil {
		c.datasetETag = string(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if raw, err := os.ReadFile(filepath.Join(c.storageDir, "rules.etag")); err == nil {
		c.rulesETag = string(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if raw, err := os.ReadFile(filepath.Join(c.storageDir, "rules.json")); err == nil {
		if err := c.engine.LoadRulesJSON(ctx, string(raw)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if raw, err := os.ReadFile(filepath.Join(c.storageDir, "dataset.json")); err == nil {
		if err := c.engine.LoadDatasetJSON(ctx, string(raw)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *Client) persistArtifact(artifact string, raw string) error {
	if c.storageDir == "" {
		return nil
	}
	var name string
	switch artifact {
	case "dataset":
		name = "dataset.json"
	case "rules":
		name = "rules.json"
	default:
		return fmt.Errorf("unknown artifact: %s", artifact)
	}
	return os.WriteFile(filepath.Join(c.storageDir, name), []byte(raw), 0o644)
}

func (c *Client) persistETags() error {
	if c.storageDir == "" {
		return nil
	}
	if err := os.WriteFile(filepath.Join(c.storageDir, "dataset.etag"), []byte(c.datasetETag), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.storageDir, "rules.etag"), []byte(c.rulesETag), 0o644); err != nil {
		return err
	}
	return nil
}
