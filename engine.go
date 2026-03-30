// Package nuglabs loads the shared nuglabs_core.wasm (built from ../core) via wazero.
package nuglabs

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const (
	// NugLabsAPIOrigin is the canonical production API origin.
	NugLabsAPIOrigin = "https://strains.nuglabs.co"
	// StrainsDatasetURL is the dataset sync endpoint.
	StrainsDatasetURL = NugLabsAPIOrigin + "/api/v1/strains"
	// RulesURL is the normalization rules sync endpoint.
	RulesURL = NugLabsAPIOrigin + "/api/v1/strains/rules"
)

// Engine wraps the WASM instance and engine handle.
type Engine struct {
	runtime wazero.Runtime
	mod     api.Module
	handle  uint32
	mem     api.Memory
}

// wasmPathEnv, when set, makes [Load] read that file instead of the embedded module (for local engine experiments).
const wasmPathEnv = "NUGLABS_WASM_PATH"

// LoadEmbedded constructs an [Engine] from the `nuglabs_core.wasm` file shipped inside this module (`wasm/`).
// Refresh that file after rebuilding Rust: `cp ../npm/wasm/nuglabs_core.wasm wasm/` (or run `npm run build:wasm` in `../npm` then copy).
func LoadEmbedded(ctx context.Context) (*Engine, error) {
	return LoadWasm(ctx, embeddedWasm)
}

// Load is the usual entrypoint: it uses the embedded WASM. If the environment variable `NUGLABS_WASM_PATH`
// is set, that path is loaded instead (optional override for development).
func Load(ctx context.Context) (*Engine, error) {
	if p := os.Getenv(wasmPathEnv); p != "" {
		return LoadWasmFile(ctx, p)
	}
	return LoadEmbedded(ctx)
}

// LoadWasmFile reads a `.wasm` from disk. Most callers should use [Load] or [LoadEmbedded].
func LoadWasmFile(ctx context.Context, path string) (*Engine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadWasm(ctx, raw)
}

// LoadWasm instantiates the module from raw bytes (advanced / tests).
func LoadWasm(ctx context.Context, wasm []byte) (*Engine, error) {
	r := wazero.NewRuntime(ctx)
	mod, err := r.Instantiate(ctx, wasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, err
	}

	mem := mod.Memory()
	if mem == nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("nuglabs_core.wasm: no memory export")
	}

	create := mod.ExportedFunction("nuglabs_engine_create")
	res, err := create.Call(ctx)
	if err != nil || len(res) == 0 {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("nuglabs_engine_create: %w", err)
	}

	return &Engine{runtime: r, mod: mod, handle: uint32(res[0]), mem: mem}, nil
}

// Close releases the runtime.
func (e *Engine) Close(ctx context.Context) error {
	if e == nil || e.runtime == nil {
		return nil
	}
	destroy := e.mod.ExportedFunction("nuglabs_engine_destroy")
	if destroy != nil {
		_, _ = destroy.Call(ctx, uint64(e.handle))
	}
	return e.runtime.Close(ctx)
}

func (e *Engine) loadDataset(ctx context.Context, json string) error {
	return e.writeStringCall(ctx, "nuglabs_engine_load_dataset", json)
}

func (e *Engine) loadRules(ctx context.Context, json string) error {
	return e.writeStringCall(ctx, "nuglabs_engine_load_rules", json)
}

func (e *Engine) writeStringCall(ctx context.Context, name string, payload string) error {
	alloc := e.mod.ExportedFunction("nuglabs_alloc")
	fn := e.mod.ExportedFunction(name)
	dealloc := e.mod.ExportedFunction("nuglabs_dealloc")
	if alloc == nil || fn == nil || dealloc == nil {
		return fmt.Errorf("missing export: alloc/%s/dealloc", name)
	}

	b := []byte(payload)
	ptrRes, err := alloc.Call(ctx, uint64(len(b)))
	if err != nil || len(ptrRes) == 0 {
		return fmt.Errorf("alloc: %w", err)
	}
	ptr := uint32(ptrRes[0])
	if !e.mem.Write(ptr, b) {
		_, _ = dealloc.Call(ctx, uint64(ptr), uint64(len(b)))
		return fmt.Errorf("memory write failed")
	}

	code, err := fn.Call(ctx, uint64(e.handle), uint64(ptr), uint64(len(b)))
	_, _ = dealloc.Call(ctx, uint64(ptr), uint64(len(b)))
	if err != nil {
		return err
	}
	if len(code) > 0 && code[0] != 0 {
		return fmt.Errorf("%s failed: %d", name, code[0])
	}
	return nil
}

// LoadDatasetJSON loads the strain array JSON into the engine.
func (e *Engine) LoadDatasetJSON(ctx context.Context, datasetJSON string) error {
	return e.loadDataset(ctx, datasetJSON)
}

// LoadRulesJSON loads normalization rules JSON into the engine.
func (e *Engine) LoadRulesJSON(ctx context.Context, rulesJSON string) error {
	return e.loadRules(ctx, rulesJSON)
}

// LoadBundledDataset loads the wrapper-shipped baseline dataset JSON into WASM.
func (e *Engine) LoadBundledDataset(ctx context.Context) error {
	return e.loadDataset(ctx, embeddedDatasetJSON)
}

// LoadBundledRules loads the wrapper-shipped baseline rules JSON into WASM.
func (e *Engine) LoadBundledRules(ctx context.Context) error {
	return e.loadRules(ctx, embeddedRulesJSON)
}

// TickActions returns due sync actions as artifact keys ("dataset", "rules").
func (e *Engine) TickActions(ctx context.Context, nowMs int64) ([]string, error) {
	if fn := e.mod.ExportedFunction("nuglabs_engine_tick_actions"); fn != nil {
		maskRes, err := fn.Call(ctx, uint64(e.handle), uint64(nowMs))
		if err != nil {
			return nil, err
		}
		mask := uint64(0)
		if len(maskRes) > 0 {
			mask = maskRes[0]
		}
		out := make([]string, 0, 2)
		if (mask & 1) != 0 {
			out = append(out, "dataset")
		}
		if (mask & 2) != 0 {
			out = append(out, "rules")
		}
		return out, nil
	}

	// Backward compatibility for older WASM binaries.
	tick := e.mod.ExportedFunction("nuglabs_engine_tick")
	if tick == nil {
		return nil, fmt.Errorf("missing export: nuglabs_engine_tick_actions")
	}
	res, err := tick.Call(ctx, uint64(e.handle), uint64(nowMs))
	if err != nil {
		return nil, err
	}
	if len(res) > 0 && res[0] != 0 {
		return []string{"dataset", "rules"}, nil
	}
	return nil, nil
}

// GetStrain returns a strain object or nil if not found.
func (e *Engine) GetStrain(ctx context.Context, name string) (map[string]any, error) {
	raw, err := e.callJSONOut(ctx, "nuglabs_engine_get_strain", name)
	if err != nil {
		return nil, err
	}
	if raw == "null" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAllStrains returns all strains currently loaded in the engine.
func (e *Engine) GetAllStrains(ctx context.Context) ([]map[string]any, error) {
	raw, err := e.callJSONOutNoInput(ctx, "nuglabs_engine_get_all_strains")
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) callJSONOutNoInput(ctx context.Context, fnName string) (string, error) {
	alloc := e.mod.ExportedFunction("nuglabs_alloc")
	call := e.mod.ExportedFunction(fnName)
	dealloc := e.mod.ExportedFunction("nuglabs_dealloc")
	if alloc == nil || call == nil || dealloc == nil {
		return "", fmt.Errorf("missing exports for %s", fnName)
	}

	outPtrSlot, err := alloc.Call(ctx, 4)
	if err != nil {
		return "", err
	}
	outLenSlot, err := alloc.Call(ctx, 4)
	if err != nil {
		_, _ = dealloc.Call(ctx, outPtrSlot[0], 4)
		return "", err
	}

	op := uint32(outPtrSlot[0])
	ol := uint32(outLenSlot[0])
	st, err := call.Call(ctx, uint64(e.handle), uint64(op), uint64(ol))
	if err != nil {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", err
	}
	if len(st) > 0 && st[0] != 0 {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("%s status %d", fnName, st[0])
	}

	ptrSlot, ok := e.mem.Read(op, 4)
	if !ok {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("read out ptr slot")
	}
	lenSlot, ok := e.mem.Read(ol, 4)
	if !ok {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("read out len slot")
	}

	rp := binary.LittleEndian.Uint32(ptrSlot)
	rl := binary.LittleEndian.Uint32(lenSlot)
	if rp == 0 || rl == 0 || rl > 50_000_000 {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("invalid out pointers for %s", fnName)
	}
	payload, ok := e.mem.Read(rp, rl)
	if !ok || len(payload) == 0 || !json.Valid(payload) {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("invalid json out for %s", fnName)
	}
	out := string(append([]byte(nil), payload...))
	_, _ = dealloc.Call(ctx, uint64(rp), uint64(rl))
	_, _ = dealloc.Call(ctx, uint64(op), 4)
	_, _ = dealloc.Call(ctx, uint64(ol), 4)
	return out, nil
}

// SearchStrains performs a normalized partial search across names and aliases.
func (e *Engine) SearchStrains(ctx context.Context, query string) ([]map[string]any, error) {
	raw, err := e.callJSONOut(ctx, "nuglabs_engine_search", query)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) callJSONOut(ctx context.Context, fnName string, input string) (string, error) {
	alloc := e.mod.ExportedFunction("nuglabs_alloc")
	call := e.mod.ExportedFunction(fnName)
	dealloc := e.mod.ExportedFunction("nuglabs_dealloc")
	if alloc == nil || call == nil || dealloc == nil {
		return "", fmt.Errorf("missing exports for %s", fnName)
	}

	inb := []byte(input)
	inPtrRes, err := alloc.Call(ctx, uint64(len(inb)))
	if err != nil {
		return "", err
	}
	inPtr := uint32(inPtrRes[0])
	if !e.mem.Write(inPtr, inb) {
		_, _ = dealloc.Call(ctx, uint64(inPtr), uint64(len(inb)))
		return "", fmt.Errorf("write input")
	}

	outPtrSlot, err := alloc.Call(ctx, 4)
	if err != nil {
		_, _ = dealloc.Call(ctx, uint64(inPtr), uint64(len(inb)))
		return "", err
	}
	outLenSlot, err := alloc.Call(ctx, 4)
	if err != nil {
		_, _ = dealloc.Call(ctx, uint64(inPtr), uint64(len(inb)))
		_, _ = dealloc.Call(ctx, outPtrSlot[0], 4)
		return "", err
	}

	op := uint32(outPtrSlot[0])
	ol := uint32(outLenSlot[0])

	st, err := call.Call(ctx, uint64(e.handle), uint64(inPtr), uint64(len(inb)), uint64(op), uint64(ol))
	_, _ = dealloc.Call(ctx, uint64(inPtr), uint64(len(inb)))
	if err != nil {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", err
	}
	if len(st) > 0 && st[0] != 0 {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("%s status %d", fnName, st[0])
	}

	ptrSlot, ok := e.mem.Read(op, 4)
	if !ok {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("read out ptr slot")
	}
	lenSlot, ok := e.mem.Read(ol, 4)
	if !ok {
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return "", fmt.Errorf("read out len slot")
	}

	a1 := binary.LittleEndian.Uint32(ptrSlot)
	a2 := binary.LittleEndian.Uint32(lenSlot)
	candidates := []struct{ rp, rl uint32 }{
		{a1, a2},
		{a2, a1},
	}

	for _, c := range candidates {
		if c.rp == 0 || c.rl == 0 || c.rl > 50_000_000 {
			continue
		}
		payload, ok := e.mem.Read(c.rp, c.rl)
		if !ok || len(payload) == 0 || !json.Valid(payload) {
			continue
		}
		// Copy before nuglabs_dealloc: the slice aliases WASM memory that dealloc may overwrite.
		out := string(append([]byte(nil), payload...))
		_, _ = dealloc.Call(ctx, uint64(c.rp), uint64(c.rl))
		_, _ = dealloc.Call(ctx, uint64(op), 4)
		_, _ = dealloc.Call(ctx, uint64(ol), 4)
		return out, nil
	}

	_, _ = dealloc.Call(ctx, uint64(op), 4)
	_, _ = dealloc.Call(ctx, uint64(ol), 4)
	return "", fmt.Errorf("could not decode wasm json output (a1=%d a2=%d)", a1, a2)
}
