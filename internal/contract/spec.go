// Package contract reads a part's OpenAPI spec and checks a running service
// against it: `ygg check <part> <url>`. The built-in parts pass it in their
// tests, and a replacement written in anything else passes the same checks
// before it takes a part's place (docs/replacing-a-part.md).
//
// The schema checker covers what the specs use: $ref, type (also a list),
// required, properties, additionalProperties, items, enum, minItems and
// maxItems. Formats are not checked.
package contract

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is one part's contract.
type Spec struct {
	Part string
	root map[string]any
}

// Load reads a spec.
func Load(part string, data []byte) (*Spec, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s spec: %w", part, err)
	}
	if _, ok := root["paths"].(map[string]any); !ok {
		return nil, fmt.Errorf("%s spec has no paths", part)
	}
	return &Spec{Part: part, root: root}, nil
}

var methods = []string{"get", "post", "put", "patch", "delete"}

// Routes is every operation in the spec as "METHOD /path", sorted.
func (s *Spec) Routes() []string {
	var routes []string
	for path, item := range s.root["paths"].(map[string]any) {
		operations, _ := item.(map[string]any)
		for _, method := range methods {
			if _, ok := operations[method]; ok {
				routes = append(routes, strings.ToUpper(method)+" "+path)
			}
		}
	}
	slices.Sort(routes)
	return routes
}

// CompareRoutes says which routes a part serves that its spec lacks, and the
// other way round. Empty when they match.
func (s *Spec) CompareRoutes(served []string) []string {
	want := map[string]bool{}
	for _, route := range s.Routes() {
		want[route] = true
	}
	have := map[string]bool{}
	var problems []string
	for _, route := range served {
		have[route] = true
		if !want[route] {
			problems = append(problems, "served but not in the spec: "+route)
		}
	}
	for _, route := range s.Routes() {
		if !have[route] {
			problems = append(problems, "in the spec but not served: "+route)
		}
	}
	slices.Sort(problems)
	return problems
}

func (s *Spec) operation(method, path string) (map[string]any, bool) {
	item, _ := s.root["paths"].(map[string]any)[path].(map[string]any)
	operation, ok := item[strings.ToLower(method)].(map[string]any)
	return operation, ok
}

// Response is the schema and content type a status of an operation answers
// with. The schema is nil for a response without a JSON body.
func (s *Spec) Response(method, path string, status int) (schema any, contentType string, ok bool) {
	operation, found := s.operation(method, path)
	if !found {
		return nil, "", false
	}
	responses, _ := operation["responses"].(map[string]any)
	response, found := responses[strconv.Itoa(status)]
	if !found {
		return nil, "", false
	}
	resolved, err := s.resolve(response)
	if err != nil {
		return nil, "", false
	}
	content, _ := resolved.(map[string]any)["content"].(map[string]any)
	for kind, body := range content {
		contentType = kind
		if kind == "application/json" {
			return body.(map[string]any)["schema"], kind, true
		}
	}
	return nil, contentType, true
}

// Secured is whether an operation needs the bearer token.
func (s *Spec) Secured(method, path string) bool {
	operation, _ := s.operation(method, path)
	if security, ok := operation["security"].([]any); ok {
		return len(security) > 0
	}
	global, _ := s.root["security"].([]any)
	return len(global) > 0
}

// resolve follows a local $ref ("#/components/schemas/Alert").
func (s *Spec) resolve(node any) (any, error) {
	for range 16 {
		object, ok := node.(map[string]any)
		if !ok {
			return node, nil
		}
		ref, ok := object["$ref"].(string)
		if !ok {
			return node, nil
		}
		target, found := strings.CutPrefix(ref, "#/")
		if !found {
			return nil, fmt.Errorf("only local $refs are supported, not %s", ref)
		}
		var current any = s.root
		for _, segment := range strings.Split(target, "/") {
			current, ok = current.(map[string]any)[segment]
			if !ok {
				return nil, fmt.Errorf("$ref %s points nowhere", ref)
			}
		}
		node = current
	}
	return nil, fmt.Errorf("$ref loop")
}

// Validate checks a decoded JSON value against a schema from the spec.
func (s *Spec) Validate(schema, value any) error {
	return s.validate(schema, value, "$")
}

func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

func (s *Spec) validate(schema, value any, at string) error {
	resolved, err := s.resolve(schema)
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	object, ok := resolved.(map[string]any)
	if !ok {
		return nil
	}
	if enum, ok := object["enum"].([]any); ok && !slices.ContainsFunc(enum, func(option any) bool { return fmt.Sprint(option) == fmt.Sprint(value) }) {
		return fmt.Errorf("%s: %v is not one of %v", at, value, enum)
	}
	var types []string
	switch t := object["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, one := range t {
			types = append(types, fmt.Sprint(one))
		}
	}
	if len(types) > 0 && !slices.ContainsFunc(types, func(t string) bool { return isType(t, value) }) {
		return fmt.Errorf("%s: want %s, got %s", at, strings.Join(types, " or "), describe(value))
	}
	switch v := value.(type) {
	case map[string]any:
		required, _ := object["required"].([]any)
		for _, name := range required {
			if _, present := v[fmt.Sprint(name)]; !present {
				return fmt.Errorf("%s: missing %s", at, name)
			}
		}
		properties, _ := object["properties"].(map[string]any)
		for name, field := range v {
			if property, known := properties[name]; known {
				if err := s.validate(property, field, at+"."+name); err != nil {
					return err
				}
			} else if extra, isSchema := object["additionalProperties"].(map[string]any); isSchema {
				if err := s.validate(extra, field, at+"."+name); err != nil {
					return err
				}
			}
		}
	case []any:
		if least, ok := number(object["minItems"]); ok && float64(len(v)) < least {
			return fmt.Errorf("%s: %d items, want at least %v", at, len(v), least)
		}
		if most, ok := number(object["maxItems"]); ok && float64(len(v)) > most {
			return fmt.Errorf("%s: %d items, want at most %v", at, len(v), most)
		}
		if items, ok := object["items"]; ok {
			for i, item := range v {
				if err := s.validate(items, item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isType(t string, value any) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := number(value)
		return ok
	case "integer":
		n, ok := number(value)
		return ok && n == math.Trunc(n)
	case "null":
		return value == nil
	}
	return false
}

func describe(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%v", value)
}
