package munin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
	"yggdrasil/munin/prod"
	"yggdrasil/munin/release"
	"yggdrasil/munin/source"
	"yggdrasil/munin/tcgpriser"
)

// Sources is every source Munin can run, by the name MUNIN_SOURCES uses.
var Sources = map[string]source.Factory{
	tcgpriser.Name: tcgpriser.New,
	release.Name:   release.New,
	prod.Name:      prod.New,
}

// SourceNames is the names in Sources, sorted.
func SourceNames() []string {
	names := make([]string, 0, len(Sources))
	for name := range Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MakeSources makes the named sources, reading each one's settings with
// lookup (MUNIN_<NAME>_<SETTING>).
func MakeSources(names []string, lookup func(string) string) ([]source.Source, error) {
	var sources []source.Source
	for _, name := range names {
		factory, ok := Sources[name]
		if !ok {
			return nil, fmt.Errorf("MUNIN_SOURCES: no source %q; there are %s", name, strings.Join(SourceNames(), ", "))
		}
		prefix := "MUNIN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
		made, err := factory(func(setting string) string { return strings.TrimSpace(lookup(prefix + setting)) })
		if err != nil {
			return nil, err
		}
		sources = append(sources, made)
	}
	return sources, nil
}

// SourceSeen is a source as Munin last saw it.
type SourceSeen struct {
	Name      string    `json:"name"`
	Up        bool      `json:"up"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	Data      any       `json:"data,omitempty"`
}

// sourceKey is a source's check on Gjallar.
func sourceKey(name, key string) string { return name + ":" + key }

// feedKey is an alert from a source's own feed on Gjallar.
func feedKey(name, key string) string { return "feed:" + name + ":" + key }

// isFeedKey is a key Munin raises from a feed, outside the watch: the watch
// would otherwise take it back after a restart and resolve it as unchecked.
func isFeedKey(key string) bool { return strings.HasPrefix(key, "feed:") }

// sourceChecked is whether a key is a check of a source that runs.
func (m *Munin) sourceChecked(key string) bool {
	return slices.ContainsFunc(m.Sources, func(s source.Source) bool { return strings.HasPrefix(key, s.Name()+":") })
}

// poll runs one source once and tells Gjallar.
func (m *Munin) poll(ctx context.Context, s source.Source) error {
	if offline, ok := m.watch.Get(OfflineKey); ok && offline.Down {
		// Its checks stay as they were, as the internet targets do.
		return nil
	}
	m.mu.Lock()
	cursor := m.state.Cursors[s.Name()]
	m.mu.Unlock()

	report := s.Poll(ctx, cursor)

	results := make([]watch.Result, len(report.Checks))
	for i, check := range report.Checks {
		check.Key = sourceKey(s.Name(), check.Key)
		results[i] = check
	}
	m.watch.Observe(results)
	m.watch.Forget(m.checked)
	err := m.watch.Sync(ctx, m.Gjallar)

	delivered := m.deliver(ctx, s.Name(), report.Events)
	if err == nil {
		err = delivered
	}
	if report.Cursor != "" && (delivered == nil || errors.Is(delivered, alert.ErrOff)) {
		m.keepCursor(s.Name(), report.Cursor)
	}

	m.mu.Lock()
	m.sourcesSeen[s.Name()] = SourceSeen{Name: s.Name(), Up: report.Up, Detail: report.Detail, CheckedAt: m.now(), Data: report.Data}
	m.mu.Unlock()
	if errors.Is(err, alert.ErrOff) {
		return nil
	}
	return err
}

// deliver raises or resolves each event in order, and stops at the first
// Gjallar refuses so none is skipped: the next poll reads them again.
func (m *Munin) deliver(ctx context.Context, name string, events []source.Event) error {
	for _, event := range events {
		key := feedKey(name, event.Key)
		var err error
		switch {
		case event.Resolved:
			err = m.Gjallar.Resolve(ctx, key)
		case event.ExpiresAt != nil && !event.ExpiresAt.After(m.now()):
			// Already over: raising it would only push it.
			continue
		default:
			err = m.Gjallar.Raise(ctx, alert.Raise{
				Key: key, Severity: event.Severity, Title: event.Title,
				Detail: event.Detail, Link: event.Link, ExpiresAt: event.ExpiresAt,
			})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// State is what Munin keeps across restarts: where each feed got to.
type State struct {
	Cursors map[string]string `json:"cursors"`
}

// LoadState reads the state file; a missing one is an empty state.
func LoadState(path string) (State, error) {
	state := State{Cursors: map[string]string{}}
	if path == "" {
		return state, nil
	}
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(text, &state); err != nil {
		return state, fmt.Errorf("%s: %w", path, err)
	}
	if state.Cursors == nil {
		state.Cursors = map[string]string{}
	}
	return state, nil
}

func (m *Munin) keepCursor(name, cursor string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Cursors[name] == cursor {
		return
	}
	m.state.Cursors[name] = cursor
	if m.StatePath == "" {
		return
	}
	text, _ := json.MarshalIndent(m.state, "", "  ")
	// Written aside and renamed, so a crash never leaves half a file.
	if err := os.MkdirAll(filepath.Dir(m.StatePath), 0o755); err == nil {
		temp := m.StatePath + ".tmp"
		if err := os.WriteFile(temp, text, 0o600); err == nil {
			_ = os.Rename(temp, m.StatePath)
		}
	}
}
