// Package where says where each part of Yggdrasil listens, and where the
// others find it. Parts never assume they talk to the built-in version of
// another part: they go to YGG_<PART>_URL, so a replacement written in
// anything is swapped in by changing one setting (docs/replacing-a-part.md).
package where

import (
	"fmt"
	"strings"

	"yggdrasil/internal/env"
)

// Ports are each part's default port on 127.0.0.1.
var Ports = map[string]int{
	"norn":       8771,
	"sindri":     8772,
	"vedrfolnir": 8773,
	"gjallar":    8774,
	"ratatoskr":  8775,
	"munin":      8776,
	"huginn":     8777,
	"dellingr":   8778,
	"hermodr":    8779,
	"loki":       8780,
}

// URL is where the other parts reach this one: YGG_<PART>_URL, or its
// default port on loopback.
func URL(part string) string {
	return env.String("YGG_"+strings.ToUpper(part)+"_URL", fmt.Sprintf("http://127.0.0.1:%d", Ports[part]))
}

// Addr is where the built-in part listens: <PART>_ADDR, or its default port
// on loopback.
func Addr(part string) string {
	return env.String(strings.ToUpper(part)+"_ADDR", fmt.Sprintf("127.0.0.1:%d", Ports[part]))
}
