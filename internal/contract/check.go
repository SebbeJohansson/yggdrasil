package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result is one check.
type Result struct {
	Name   string
	OK     bool
	Skip   bool
	Detail string
}

// Options are the tokens a check may use.
type Options struct {
	// OwnerToken reads the part's routes as you.
	OwnerToken string
	// SenderToken acts as a part: on Gjallar it raises and resolves a test alert.
	SenderToken string
	HTTP        *http.Client
}

type checker struct {
	spec    *Spec
	base    string
	options Options
	results []Result
}

// Check runs the contract against the service at base.
//
//  1. GET /health answers without a token, in the Health shape, naming the part.
//  2. GET /openapi.yaml answers without a token.
//  3. Every other GET without path parameters refuses a missing token (401)
//     and, with the owner token, answers 200 in the spec's shape.
//  4. Part-specific steps, where the part has them (Gjallar: raise, update,
//     read, resolve with the sender token; the owner may not raise).
func Check(ctx context.Context, spec *Spec, base string, options Options) []Result {
	if options.HTTP == nil {
		options.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	c := &checker{spec: spec, base: strings.TrimRight(base, "/"), options: options}
	c.health(ctx)
	c.call(ctx, "serves its spec", http.MethodGet, "/openapi.yaml", "", nil, http.StatusOK, nil)
	for _, route := range spec.Routes() {
		method, path, _ := strings.Cut(route, " ")
		if method != http.MethodGet || strings.Contains(path, "{") || path == "/health" || path == "/openapi.yaml" {
			continue
		}
		if schema, contentType, _ := spec.Response(method, path, http.StatusOK); contentType != "application/json" || schema == nil {
			continue
		}
		query, callable := spec.Query(method, path)
		if !callable {
			c.skip(route, "a required parameter has no example in the spec")
			continue
		}
		if spec.Secured(method, path) {
			c.call(ctx, "refuses no token: "+route, method, path, "", nil, http.StatusUnauthorized, nil)
		}
		if options.OwnerToken == "" {
			c.skip(route, "no owner token")
			continue
		}
		c.call(ctx, route, method, path+query, options.OwnerToken, nil, http.StatusOK, nil)
	}
	if scenario, ok := scenarios[spec.Part]; ok {
		scenario(ctx, c)
	}
	return c.results
}

// Passed is whether no check failed.
func Passed(results []Result) bool {
	for _, result := range results {
		if !result.OK && !result.Skip {
			return false
		}
	}
	return true
}

func (c *checker) pass(name string) {
	c.results = append(c.results, Result{Name: name, OK: true})
}

func (c *checker) fail(name, format string, args ...any) {
	c.results = append(c.results, Result{Name: name, Detail: fmt.Sprintf(format, args...)})
}

func (c *checker) skip(name, why string) {
	c.results = append(c.results, Result{Name: name, Skip: true, Detail: why})
}

// call makes one request, checks its status, and, for a JSON answer the spec
// describes, its shape. The decoded body is returned for later steps.
func (c *checker) call(ctx context.Context, name, method, path, token string, body any, want int, then func(any) error) any {
	var reader io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		c.fail(name, "%v", err)
		return nil
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.options.HTTP.Do(request)
	if err != nil {
		c.fail(name, "%v", err)
		return nil
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if response.StatusCode != want {
		c.fail(name, "answered %d, want %d: %s", response.StatusCode, want, clip(data))
		return nil
	}
	specPath := path
	if at := strings.IndexAny(specPath, "?"); at >= 0 {
		specPath = specPath[:at]
	}
	specPath = c.templateOf(method, specPath)
	schema, contentType, described := c.spec.Response(method, specPath, want)
	if !described {
		c.fail(name, "the spec has no %d answer for %s %s", want, method, specPath)
		return nil
	}
	var decoded any
	if contentType == "application/json" {
		if err := json.Unmarshal(data, &decoded); err != nil {
			c.fail(name, "not JSON: %s", clip(data))
			return nil
		}
		if schema != nil {
			if err := c.spec.Validate(schema, decoded); err != nil {
				c.fail(name, "%v", err)
				return nil
			}
		}
	}
	if then != nil {
		if err := then(decoded); err != nil {
			c.fail(name, "%v", err)
			return nil
		}
	}
	c.pass(name)
	return decoded
}

// templateOf finds the spec path a concrete path matches: /v1/alerts/12 is /v1/alerts/{id}.
func (c *checker) templateOf(method, path string) string {
	parts := strings.Split(path, "/")
	for _, route := range c.spec.Routes() {
		routeMethod, template, _ := strings.Cut(route, " ")
		if routeMethod != method {
			continue
		}
		pieces := strings.Split(template, "/")
		if len(pieces) != len(parts) {
			continue
		}
		match := true
		for i := range pieces {
			if pieces[i] != parts[i] && !strings.HasPrefix(pieces[i], "{") {
				match = false
				break
			}
		}
		if match {
			return template
		}
	}
	return path
}

func (c *checker) health(ctx context.Context) {
	c.call(ctx, "GET /health", http.MethodGet, "/health", "", nil, http.StatusOK, func(body any) error {
		if part := body.(map[string]any)["part"]; part != c.spec.Part {
			return fmt.Errorf("part is %v, want %s", part, c.spec.Part)
		}
		return nil
	})
}

func clip(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}

var scenarios = map[string]func(context.Context, *checker){
	"gjallar": gjallarScenario,
}

func field(value any, path ...string) any {
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

// gjallarScenario raises a test alert as a part, updates it, reads it,
// resolves it, and checks the owner may not raise.
func gjallarScenario(ctx context.Context, c *checker) {
	if c.options.OwnerToken != "" {
		c.call(ctx, "the owner may not raise", http.MethodPost, "/v1/alerts", c.options.OwnerToken,
			map[string]any{"key": "conformance", "severity": "info", "title": "x"}, http.StatusForbidden, nil)
	}
	if c.options.SenderToken == "" {
		c.skip("raise, update, read and resolve", "no sender token")
		return
	}
	key := fmt.Sprintf("conformance:%d", time.Now().UnixNano())
	raise := map[string]any{"key": key, "severity": "info", "title": "Conformance check", "detail": "ygg check; resolves itself"}
	first := c.call(ctx, "raise", http.MethodPost, "/v1/alerts", c.options.SenderToken, raise, http.StatusCreated, func(body any) error {
		if change := field(body, "change"); change != "opened" {
			return fmt.Errorf("change is %v, want opened", change)
		}
		return nil
	})
	if first == nil {
		return
	}
	id := field(first, "alert", "id")
	c.call(ctx, "raise the same key again", http.MethodPost, "/v1/alerts", c.options.SenderToken, raise, http.StatusOK, func(body any) error {
		if change := field(body, "change"); change != "updated" {
			return fmt.Errorf("change is %v, want updated", change)
		}
		if count := field(body, "alert", "count"); count != float64(2) {
			return fmt.Errorf("count is %v, want 2", count)
		}
		if again := field(body, "alert", "id"); again != id {
			return fmt.Errorf("a second alert (%v) instead of updating %v", again, id)
		}
		return nil
	})
	c.call(ctx, "raise without a token", http.MethodPost, "/v1/alerts", "", raise, http.StatusUnauthorized, nil)
	c.call(ctx, "raise a bad severity", http.MethodPost, "/v1/alerts", c.options.SenderToken,
		map[string]any{"key": key, "severity": "loud", "title": "x"}, http.StatusBadRequest, nil)
	if c.options.OwnerToken != "" {
		c.call(ctx, "GET /v1/alerts/{id}", http.MethodGet, fmt.Sprintf("/v1/alerts/%v", id), c.options.OwnerToken, nil, http.StatusOK, nil)
	}
	c.call(ctx, "resolve", http.MethodPost, "/v1/alerts/resolve", c.options.SenderToken, map[string]any{"key": key}, http.StatusOK, func(body any) error {
		if field(body, "resolved") != true {
			return fmt.Errorf("resolved is %v, want true", field(body, "resolved"))
		}
		return nil
	})
	c.call(ctx, "heartbeat", http.MethodPost, "/v1/heartbeat", c.options.SenderToken, map[string]any{}, http.StatusNoContent, nil)
}
