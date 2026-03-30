# NugLabs Go SDK

Local-first Go SDK powered by `nuglabs_core.wasm` (Rust core) via wazero.

Current Go module versioning is managed by your Go module tags.

## Design

- Embeds `wasm/nuglabs_core.wasm`
- Embeds bundled startup artifacts:
  - `dataset.json`
  - `rules.json`
- `NewClient(...)` auto-loads bundled artifacts into WASM on startup
- Performs reads/searches against local in-memory WASM state only
- Exposes canonical sync endpoints for host integrations:
  - `StrainsDatasetURL` (`https://strains.nuglabs.co/api/v1/strains`)
  - `RulesURL` (`https://strains.nuglabs.co/api/v1/strains/rules`)
- High-level client supports auto background sync + manual `ForceResync*()` calls
- Low-level engine still exposes `TickActions(...)` (`dataset`, `rules`)

## Install

```bash
go get github.com/nug-labs/go-sdk
```

## Usage (High-Level Client)

```go
package main

import (
	"context"
	"fmt"

	nuglabs "github.com/nug-labs/go-sdk"
)

func main() {
	ctx := context.Background()
	client, err := nuglabs.NewClient(ctx, nil) // auto-loads bundled data + starts background sync loop
	if err != nil {
		panic(err)
	}
	defer client.Close(ctx)

	all, _ := client.GetAllStrains(ctx)
	strain, _ := client.GetStrain(ctx, "BlueDream")
	hits, _ := client.SearchStrains(ctx, "dream")
	sync, _ := client.ForceResync(ctx) // manual dataset + rules sync

	fmt.Println(len(all), len(hits), strain["name"], sync.Dataset.Changed, sync.Rules.Changed)
}
```

```go
// Advanced options
client, err := nuglabs.NewClient(ctx, &nuglabs.ClientOptions{
	AutoSync:     true,
	SyncInterval: time.Minute,
	HTTPClient:   http.DefaultClient,
	DatasetURL:   nuglabs.StrainsDatasetURL,
	RulesURL:     nuglabs.RulesURL,
	StorageDir:   "/tmp/nuglabs",
})
if err != nil {
	panic(err)
}
defer client.Close(ctx)
```

## Usage (Low-Level Engine)

```go
engine, err := nuglabs.Load(ctx)
if err != nil {
	panic(err)
}
defer engine.Close(ctx)

if err := engine.LoadBundledRules(ctx); err != nil {
	panic(err)
}
if err := engine.LoadBundledDataset(ctx); err != nil {
	panic(err)
}

actions, _ := engine.TickActions(ctx, time.Now().UnixMilli())
_ = actions // host performs HTTP + persistence
```

## API Notes

### High-level (`Client`)

- `NewClient(ctx, opts)` loads bundled artifacts and starts background sync when `AutoSync` is true.
- `GetAllStrains(ctx)`, `GetStrain(ctx, name)`, `SearchStrains(ctx, query)` are local-only reads.
- `ForceResync(ctx)` runs dataset + rules sync and returns `SyncResult`.
- `ForceResyncDataset(ctx)` / `ForceResyncRules(ctx)` run targeted sync.
- `StorageDir` enables persistence of `dataset.json`, `rules.json`, `dataset.etag`, and `rules.etag` across process restarts.
- ETag conditional requests are used automatically when prior ETags exist.
- Rules endpoint `404` is treated as `not-modified` for backward-compatible deployments.

### Low-level (`Engine`)

- `Load(ctx)` loads embedded WASM by default.
- `LoadWasmFile(ctx, path)` loads custom `.wasm` bytes from disk.
- `LoadDatasetJSON(ctx, raw)` / `LoadRulesJSON(ctx, raw)` replace current in-memory state.
- `LoadBundledDataset(ctx)` / `LoadBundledRules(ctx)` load packaged startup artifacts.
- `GetAllStrains(ctx)` returns `[]map[string]any`.
- `GetStrain(ctx, name)` returns `map[string]any` or `nil`.
- `SearchStrains(ctx, query)` returns `[]map[string]any`.
- `TickActions(ctx, nowMs)` returns due artifacts:
  - `dataset`
  - `rules`

Typical `strain` fields from `GetStrain` include:

- `name`
- `akas`
- `type`
- `thc`
- plus any additional dataset fields included by NugLabs

## Behavior

- Exact lookup and partial search use the Rust normalization pipeline in WASM.
- Empty search queries return all currently loaded strains.
- Reads/searches never call remote APIs directly.
- High-level client performs host-managed sync in Go (`net/http`) using core scheduling.
- Low-level engine path remains host-driven (`TickActions` decides due artifacts, host performs HTTP).

## WASM overrides

- Set `NUGLABS_WASM_PATH` to load a custom `.wasm` file instead of embedded bytes.

## Build / Test

```bash
cd app/sdk/go
go test ./...
```
