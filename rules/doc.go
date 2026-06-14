//go:build ruleguard

// Package gorules defines custom ruleguard matchers for Go modernization.
//
// The matchers are loaded by golangci-lint's gocritic ruleguard checker (see
// settings.gocritic.settings.ruleguard.rules in .golangci.yaml). Every file
// carries the //go:build ruleguard tag so the normal Go toolchain ignores
// them; `go build -tags ruleguard ./rules/` compiles them as a canary.
package gorules
