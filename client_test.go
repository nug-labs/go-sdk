package nuglabs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestClientForceResyncDatasetAndRules(t *testing.T) {
	ctx := context.Background()

	dataset := embeddedDatasetJSON
	rules := embeddedRulesJSON
	dsETag := `"dataset-v1"`
	rlETag := `"rules-v1"`

	var datasetCalls int
	var rulesCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dataset":
			datasetCalls++
			if r.Header.Get("If-None-Match") == dsETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", dsETag)
			_, _ = w.Write([]byte(dataset))
		case "/rules":
			rulesCalls++
			if r.Header.Get("If-None-Match") == rlETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", rlETag)
			_, _ = w.Write([]byte(rules))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewClient(ctx, &ClientOptions{
		AutoSync:   false,
		HTTPClient: srv.Client(),
		DatasetURL: srv.URL + "/dataset",
		RulesURL:   srv.URL + "/rules",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(ctx)

	first, err := client.ForceResync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Dataset.Changed || !first.Rules.Changed {
		t.Fatalf("expected first sync to update both artifacts: %+v", first)
	}

	second, err := client.ForceResync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Dataset.Changed || second.Rules.Changed {
		t.Fatalf("expected second sync to be not-modified: %+v", second)
	}
	if datasetCalls < 2 || rulesCalls < 2 {
		t.Fatalf("expected conditional requests to execute twice: dataset=%d rules=%d", datasetCalls, rulesCalls)
	}
}

func TestClientPersistsETagAcrossRunsWhenStorageDirSet(t *testing.T) {
	ctx := context.Background()
	dsETag := `"dataset-v1"`
	rlETag := `"rules-v1"`
	storageDir := filepath.Join(t.TempDir(), "nuglabs-cache")

	var datasetCalls int
	var rulesCalls int
	var lastDatasetIfNoneMatch string
	var lastRulesIfNoneMatch string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dataset":
			datasetCalls++
			lastDatasetIfNoneMatch = r.Header.Get("If-None-Match")
			if lastDatasetIfNoneMatch == dsETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", dsETag)
			_, _ = w.Write([]byte(embeddedDatasetJSON))
		case "/rules":
			rulesCalls++
			lastRulesIfNoneMatch = r.Header.Get("If-None-Match")
			if lastRulesIfNoneMatch == rlETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", rlETag)
			_, _ = w.Write([]byte(embeddedRulesJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clientA, err := NewClient(ctx, &ClientOptions{
		AutoSync:   false,
		HTTPClient: srv.Client(),
		DatasetURL: srv.URL + "/dataset",
		RulesURL:   srv.URL + "/rules",
		StorageDir: storageDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := clientA.ForceResync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Dataset.Changed || !first.Rules.Changed {
		t.Fatalf("expected first run to update artifacts: %+v", first)
	}
	if err := clientA.Close(ctx); err != nil {
		t.Fatal(err)
	}

	clientB, err := NewClient(ctx, &ClientOptions{
		AutoSync:   false,
		HTTPClient: srv.Client(),
		DatasetURL: srv.URL + "/dataset",
		RulesURL:   srv.URL + "/rules",
		StorageDir: storageDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Close(ctx)

	second, err := clientB.ForceResync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Dataset.Changed || second.Rules.Changed {
		t.Fatalf("expected second run to return not-modified: %+v", second)
	}
	if lastDatasetIfNoneMatch != dsETag || lastRulesIfNoneMatch != rlETag {
		t.Fatalf("expected persisted etags to be reused: dataset=%q rules=%q", lastDatasetIfNoneMatch, lastRulesIfNoneMatch)
	}
	if datasetCalls < 2 || rulesCalls < 2 {
		t.Fatalf("expected two calls for each artifact across runs: dataset=%d rules=%d", datasetCalls, rulesCalls)
	}
}
