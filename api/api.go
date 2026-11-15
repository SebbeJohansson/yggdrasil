// Package api holds each part's contract: an OpenAPI 3.1 spec per part.
//
// A spec is the whole contract. Any program that serves it, and passes
// `ygg check <part> <url>`, can stand in for the built-in part
// (docs/replacing-a-part.md). Each built-in part serves its own spec at
// GET /openapi.yaml.
package api

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed *.yaml
var specs embed.FS

// Spec is a part's contract; nil when there is none.
func Spec(part string) []byte {
	data, err := specs.ReadFile(part + ".yaml")
	if err != nil {
		return nil
	}
	return data
}

// Parts is every part that has a contract.
func Parts() []string {
	entries, _ := fs.ReadDir(specs, ".")
	var parts []string
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".yaml"); ok {
			parts = append(parts, name)
		}
	}
	return parts
}
