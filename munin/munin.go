// Package munin checks the systems outside Yggdrasil that the parts depend
// on, named for Odin's raven who flies out each day and reports back. It
// fixes nothing, holds no one's credentials, and raises alerts through Gjallar.
//
// What is whose (docs/architecture.md):
//   - reachable or not: Munin ("Ollama doesn't answer")
//   - credentials: the part that holds them (an expired Slack sign-in is Norn's)
//   - resource levels: Veðrfölnir
//   - Yggdrasil's own parts: Ratatoskr
//
// Kinds of target:
//
//	http        up when anything answers: a 404 is a server that is there
//	ollama      /api/tags answers, and the models the parts need are pulled
//	openai      an OpenAI-compatible /v1/models (XeStrata) answers, and has a model loaded
//	statuspage  a provider's Statuspage JSON says there is no incident
//
// Local targets (loopback) are checked every minute, internet targets every
// 5 minutes. When the internet itself is down that is one alert, `offline`,
// and the internet targets are left as they were until it is back.
//
// Routes:
//
//	GET /health
//	GET /v1/status   every target as last seen, and every check
package munin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/watch"
)

// Name is this part's name, and its source on Gjallar.
const Name = "munin"

// Kind is how a target is checked.
type Kind string

const (
	HTTP       Kind = "http"
	Ollama     Kind = "ollama"
	OpenAI     Kind = "openai"
	Statuspage Kind = "statuspage"
)

// Target is one outside system.
type Target struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	URL  string `json:"url"`
	// Local is a server on this machine.
	Local bool `json:"local"`
}

// ParseTarget reads `kind:url` for a name.
func ParseTarget(name, text string) (Target, error) {
	kind, rawURL, ok := strings.Cut(text, ":")
	if !ok || !slices.Contains([]Kind{HTTP, Ollama, OpenAI, Statuspage}, Kind(kind)) {
		return Target{}, fmt.Errorf("%s: want kind:url with kind http, ollama, openai or statuspage, not %q", name, text)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Target{}, fmt.Errorf("%s: %q is not an http(s) URL", name, rawURL)
	}
	return Target{Name: name, Kind: Kind(kind), URL: rawURL, Local: isLocal(parsed.Hostname())}, nil
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Seen is a target as Munin last saw it.
type Seen struct {
	Target
	Up        bool      `json:"up"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

// Munin is the running checker.
type Munin struct {
	Targets []Target
	// Models are what Ollama must have pulled for the parts to work.
	Models []string
	// OnlineURL is fetched first: when it fails, the internet is down.
	OnlineURL string
	Gjallar   *alert.Client
	Local     *http.Client
	Internet  *http.Client

	watch *watch.Watch
	mu    sync.Mutex
	seen  map[string]Seen
	now   func() time.Time
}

// New makes a checker.
func New(targets []Target, models []string, onlineURL string, gjallar *alert.Client) *Munin {
	return &Munin{
		Targets:   targets,
		Models:    models,
		OnlineURL: onlineURL,
		Gjallar:   gjallar,
		Local:     &http.Client{Timeout: 2 * time.Second},
		Internet:  &http.Client{Timeout: 5 * time.Second},
		watch:     watch.New(),
		seen:      map[string]Seen{},
		now:       time.Now,
	}
}

// Mount adds the routes and loops to the part.
func (m *Munin) Mount(p *part.Part) {
	p.Handle("GET /v1/status", m.status)
	p.Loop("local", time.Minute, func(ctx context.Context) error { return m.round(ctx, true) })
	p.Loop("internet", 5*time.Minute, func(ctx context.Context) error { return m.round(ctx, false) })
}

// OfflineKey is the one alert for the internet being down.
const OfflineKey = "offline"

// round checks the local targets, or the internet ones, then tells Gjallar.
func (m *Munin) round(ctx context.Context, local bool) error {
	var results []watch.Result
	online := true
	if !local {
		err := m.reach(ctx, m.Internet, m.OnlineURL)
		online = err == nil
		results = append(results, watch.Result{
			Key: OfflineKey, OK: online, Severity: alert.Warning,
			Title: "This machine can't reach the internet", Detail: errorText(err),
		})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, target := range m.Targets {
		if target.Local != local {
			continue
		}
		if !online {
			mu.Lock()
			results = append(results, skipAll(target)...)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, seen := m.check(ctx, target)
			mu.Lock()
			results = append(results, found...)
			mu.Unlock()
			m.mu.Lock()
			m.seen[target.Name] = seen
			m.mu.Unlock()
		}()
	}
	wg.Wait()
	m.watch.Observe(results)
	// A target taken out of the settings is dropped, and its alerts resolved.
	m.watch.Forget(m.checked)
	err := m.watch.Sync(ctx, m.Gjallar)
	if err == alert.ErrOff {
		return nil
	}
	return err
}

// checked is whether a key belongs to a check Munin still runs.
func (m *Munin) checked(key string) bool {
	if key == OfflineKey {
		return true
	}
	for _, target := range m.Targets {
		if slices.Contains(keys(target), key) {
			return true
		}
	}
	return false
}

// keys is every check a target can have.
func keys(target Target) []string {
	keys := []string{downKey(target.Name)}
	switch target.Kind {
	case Ollama, OpenAI:
		keys = append(keys, modelsKey(target.Name))
	case Statuspage:
		keys = append(keys, incidentKey(target.Name))
	}
	return keys
}

// skipAll skips every check of a target, for a round while offline.
func skipAll(target Target) []watch.Result {
	keys := keys(target)
	results := make([]watch.Result, len(keys))
	for i, key := range keys {
		results[i] = watch.Result{Key: key, Skip: true}
	}
	return results
}

func downKey(name string) string     { return "down:" + name }
func modelsKey(name string) string   { return "models:" + name }
func incidentKey(name string) string { return "incident:" + name }

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (m *Munin) client(target Target) *http.Client {
	if target.Local {
		return m.Local
	}
	return m.Internet
}

// reach is up when anything answers.
func (m *Munin) reach(ctx context.Context, client *http.Client, rawURL string) error {
	_, _, err := m.get(ctx, client, rawURL)
	return err
}

func (m *Munin) get(ctx context.Context, client *http.Client, rawURL string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("User-Agent", "yggdrasil-munin")
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return response.StatusCode, body, err
}

// check is one target's results this round.
func (m *Munin) check(ctx context.Context, target Target) ([]watch.Result, Seen) {
	seen := Seen{Target: target, CheckedAt: m.now()}
	down := watch.Result{Key: downKey(target.Name), OK: true, Severity: alert.Warning, Title: target.Name + " doesn't answer"}
	status, body, err := m.get(ctx, m.client(target), target.URL)
	if err != nil {
		down.OK, down.Detail = false, err.Error()
		seen.Detail = err.Error()
		results := []watch.Result{down}
		// Its other checks wait until it answers: one problem, one alert.
		for _, skipped := range skipAll(target)[1:] {
			results = append(results, skipped)
		}
		return results, seen
	}
	seen.Up = true
	seen.Detail = fmt.Sprintf("answers %d", status)
	results := []watch.Result{down}
	switch target.Kind {
	case Ollama:
		missing, err := MissingModels(body, m.Models)
		result := watch.Result{Key: modelsKey(target.Name), OK: err == nil && len(missing) == 0, Severity: alert.Warning}
		switch {
		case err != nil:
			result.Title, result.Detail = target.Name+" did not list its models", err.Error()
		case len(missing) > 0:
			result.Title = fmt.Sprintf("%s is missing %s", target.Name, strings.Join(missing, ", "))
			result.Detail = "pull them with: ollama pull " + strings.Join(missing, " && ollama pull ")
			seen.Detail = result.Title
		}
		results = append(results, result)
	case OpenAI:
		loaded, err := LoadedModels(body)
		result := watch.Result{Key: modelsKey(target.Name), OK: err == nil && len(loaded) > 0, Severity: alert.Warning}
		switch {
		case err != nil:
			result.Title, result.Detail = target.Name+" did not list its models", err.Error()
		case len(loaded) == 0:
			result.Title = target.Name + " has no model loaded"
			result.Detail = "it answers, but every answer will wait for a model to load, or fail"
			seen.Detail = result.Title
		default:
			seen.Detail = "loaded: " + strings.Join(loaded, ", ")
		}
		results = append(results, result)
	case Statuspage:
		indicator, description, err := Incident(body)
		result := watch.Result{Key: incidentKey(target.Name), OK: err == nil && indicator == "none", Severity: alert.Warning}
		if err != nil {
			result.Title, result.Detail = target.Name+"'s status page could not be read", err.Error()
		} else {
			result.Title, result.Detail = fmt.Sprintf("%s reports an incident: %s", target.Name, description), "indicator: "+indicator
			seen.Detail = description
		}
		if indicator == "minor" {
			result.Severity = alert.Info
		}
		results = append(results, result)
	}
	return results, seen
}

// MissingModels is the wanted models Ollama's /api/tags does not list. A
// wanted name without a tag means `:latest`.
func MissingModels(tags []byte, wanted []string) ([]string, error) {
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(tags, &parsed); err != nil {
		return nil, fmt.Errorf("not Ollama's model list: %w", err)
	}
	have := map[string]bool{}
	for _, model := range parsed.Models {
		have[model.Name] = true
	}
	var missing []string
	for _, name := range wanted {
		full := name
		if !strings.Contains(name, ":") {
			full = name + ":latest"
		}
		if !have[full] && !have[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// LoadedModels is the models an OpenAI-compatible /v1/models lists as loaded.
// A server that gives no status (plain OpenAI) counts every model it lists.
func LoadedModels(body []byte) ([]string, error) {
	var parsed struct {
		Data []struct {
			ID     string `json:"id"`
			Status *struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("not a /v1/models list: %w", err)
	}
	var loaded []string
	for _, model := range parsed.Data {
		if model.Status == nil || model.Status.Value == "loaded" {
			loaded = append(loaded, model.ID)
		}
	}
	return loaded, nil
}

// Incident reads a Statuspage /api/v2/status.json: its indicator (none,
// minor, major, critical) and description.
func Incident(body []byte) (string, string, error) {
	var parsed struct {
		Status struct {
			Indicator   string `json:"indicator"`
			Description string `json:"description"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Status.Indicator == "" {
		return "", "", fmt.Errorf("not a Statuspage status")
	}
	return parsed.Status.Indicator, parsed.Status.Description, nil
}

// Status is GET /v1/status's answer.
type Status struct {
	Targets []Seen        `json:"targets"`
	Checks  []watch.State `json:"checks"`
}

func (m *Munin) status(w http.ResponseWriter, _ *http.Request) {
	status := Status{Targets: []Seen{}, Checks: m.watch.States()}
	m.mu.Lock()
	for _, target := range m.Targets {
		if seen, ok := m.seen[target.Name]; ok {
			status.Targets = append(status.Targets, seen)
		} else {
			status.Targets = append(status.Targets, Seen{Target: target})
		}
	}
	m.mu.Unlock()
	part.JSON(w, http.StatusOK, status)
}
