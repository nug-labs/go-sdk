package nuglabs

import _ "embed"

//go:embed wasm/nuglabs_core.wasm
var embeddedWasm []byte

//go:embed dataset.json
var embeddedDatasetJSON string

//go:embed rules.json
var embeddedRulesJSON string
