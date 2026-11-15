// Package env reads a part's settings from its environment.
package env

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// String is the variable's trimmed value, or fallback when it is unset or blank.
func String(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Int is the variable as a whole number, or fallback when it is unset, blank
// or not a number.
func Int(name string, fallback int) int {
	if value, err := strconv.Atoi(String(name, "")); err == nil {
		return value
	}
	return fallback
}

// Duration is the variable as a Go duration ("30s", "5m"), or fallback.
func Duration(name string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(String(name, "")); err == nil && value > 0 {
		return value
	}
	return fallback
}

// List reads `a,b,c` from the variable, blanks left out.
func List(name string) []string {
	var list []string
	for _, item := range strings.Split(os.Getenv(name), ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list
}

// Pair is one `name=value` entry of a list.
type Pair struct {
	Name  string
	Value string
}

// Pairs reads `name=value,name=value`. Entries without a name or an `=` are
// left out. The value may itself hold `=` (a URL's query does).
func Pairs(text string) []Pair {
	var pairs []Pair
	for _, entry := range strings.Split(text, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if ok && name != "" && value != "" {
			pairs = append(pairs, Pair{Name: name, Value: value})
		}
	}
	return pairs
}

// Tokens reads `name=token,...` from the variable: who may call a part, by
// name. A token shorter than 32 characters is refused, since anything on this
// machine can reach loopback.
func Tokens(name string) (map[string]string, error) {
	tokens := map[string]string{}
	for _, pair := range Pairs(os.Getenv(name)) {
		if len(pair.Value) < 32 {
			return nil, fmt.Errorf("%s: the token for %q is shorter than 32 characters", name, pair.Name)
		}
		tokens[pair.Value] = pair.Name
	}
	return tokens, nil
}
