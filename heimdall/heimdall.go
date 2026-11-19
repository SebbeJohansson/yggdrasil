// Package heimdall is the security watchman. Once a night, and when you ask,
// it runs a few small checks over your repos and this machine and keeps what
// they find: vulnerable dependencies (osv.dev), listeners beyond loopback,
// secret-shaped strings in new commits, settings files others can read or
// tokens too old, and sign-ins Norn holds but nothing uses.
//
// Each check is a module (Check) that more can join. Findings persist with
// when they were first and last seen; one that is no longer found is
// resolved and kept 30 days. A high finding raises a Gjallar alert
// (<check>:<id>), resolved when it is gone.
//
// Only package names and versions leave the machine, to osv.dev, with a
// generic User-Agent. No finding ever holds a secret's value: a masked
// preview like ghp_…4f2a, the file, the line and the kind.
//
//	GET   /v1/report         ?state=active|resolved → { checkedAt, running, findings, checks }
//	POST  /v1/report/run     owner: run every check now
//	GET   /v1/settings       { at, repos, tokenAgeDays }
//	PATCH /v1/settings       owner
package heimdall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
)

// Name is this part's name, and its source on Gjallar.
const Name = "heimdall"

// Severities of a finding.
const (
	Low    = "low"
	Medium = "medium"
	High   = "high"
)

const (
	// KeepResolved is how long a resolved finding is kept.
	KeepResolved = 30 * 24 * time.Hour
	// RunTimeout is the longest one run of every check may take.
	RunTimeout = 20 * time.Minute
	// DefaultAt is when the nightly run starts, in TIME_ZONE.
	DefaultAt = "03:00"
	// DefaultTokenAgeDays is how old a token file may get before rotating it is suggested.
	DefaultTokenAgeDays = 180
	// MaxRepos is the most repos the settings may list.
	MaxRepos = 50
)

// Finding is one thing a check found.
type Finding struct {
	// ID is stable across runs, unique within its check.
	ID       string `json:"id"`
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	// Where is a file and line, an address, or a path.
	Where string `json:"where,omitempty"`
	// Fix says how to make it go away.
	Fix string `json:"fix,omitempty"`
	// Repo is the repo's name, for findings that belong to one.
	Repo       string     `json:"repo,omitempty"`
	FirstSeen  time.Time  `json:"firstSeen"`
	LastSeen   time.Time  `json:"lastSeen"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`

	// Critical makes a high finding's alert critical instead of a warning:
	// a secret in a pushed commit, or a part listening beyond loopback.
	Critical bool `json:"critical,omitempty"`
	// Commit is the commit a secret was found in, to tell when it is gone from history.
	Commit string `json:"commit,omitempty"`
}

// AlertKey is the finding's alert on Gjallar.
func (f Finding) AlertKey() string { return f.Check + ":" + f.ID }

// CheckStatus is how a check's last run went.
type CheckStatus struct {
	Name    string     `json:"name"`
	OK      bool       `json:"ok"`
	LastRun *time.Time `json:"lastRun"`
	Error   string     `json:"error,omitempty"`
	// Skipped says why the check did not run.
	Skipped string `json:"skipped,omitempty"`
}

// Report is GET /v1/report.
type Report struct {
	CheckedAt *time.Time    `json:"checkedAt"`
	Running   bool          `json:"running"`
	Findings  []Finding     `json:"findings"`
	Checks    []CheckStatus `json:"checks"`
}

// Settings is GET and PATCH /v1/settings.
type Settings struct {
	// At is when the nightly run starts, HH:MM in TIME_ZONE.
	At string `json:"at"`
	// Repos are the repos' paths. Sindri's projects are checked too.
	Repos        []string `json:"repos"`
	TokenAgeDays int      `json:"tokenAgeDays"`
}

// State is what Heimdall keeps across restarts.
type State struct {
	Settings  *Settings         `json:"settings,omitempty"`
	CheckedAt *time.Time        `json:"checkedAt,omitempty"`
	Findings  []Finding         `json:"findings"`
	Checks    []CheckStatus     `json:"checks"`
	Memory    map[string]Memory `json:"memory,omitempty"`
	// Raised are the alerts Gjallar holds for Heimdall, key to severity.
	Raised map[string]string `json:"raised,omitempty"`
}

// Repo is one repo the checks look at.
type Repo struct {
	Name string
	Path string
}

// Memory is what a check keeps between runs, by key (Secrets: each repo's
// ref tips it has read up to).
type Memory map[string][]string

// Env is what a check runs with.
type Env struct {
	Repos []Repo
	Now   time.Time
	// TokenAge is how old a token file may get.
	TokenAge time.Duration
	// Previous are the check's active findings from before.
	Previous []Finding
	// Memory is the check's own memory from the last run; a check changes it in place.
	Memory Memory
}

// Result is one check's run.
type Result struct {
	Findings []Finding
	// Skipped says why the check didn't run; its earlier findings stay as they were.
	Skipped string
}

// Check is one module of the watch.
type Check interface {
	Name() string
	Run(ctx context.Context, env *Env) (Result, error)
}

// Heimdall is the running part.
type Heimdall struct {
	Checks []Check
	// Gjallar is where high findings are raised, with Heimdall's own token.
	Gjallar *alert.Client
	// Defaults are the settings before you change them.
	Defaults Settings
	// MoreRepos are found at run time: Sindri's projects. May be nil.
	MoreRepos func(ctx context.Context) ([]Repo, error)
	// Zone is TIME_ZONE, for the nightly run.
	Zone *time.Location
	// StatePath is where everything is kept; empty keeps it in memory.
	StatePath string
	Logf      func(format string, args ...any)

	now func() time.Time

	mu      sync.Mutex
	state   State
	running bool
	adopted bool
	runs    sync.WaitGroup
	lastDay string
}

// New makes Heimdall over the checks.
func New(checks []Check, gjallar *alert.Client, defaults Settings) *Heimdall {
	return &Heimdall{
		Checks:   checks,
		Gjallar:  gjallar,
		Defaults: defaults,
		Zone:     time.Local,
		Logf:     func(string, ...any) {},
		now:      time.Now,
		state:    State{Findings: []Finding{}, Checks: []CheckStatus{}, Memory: map[string]Memory{}, Raised: map[string]string{}},
	}
}

// DefaultSettings is Heimdall before you set anything, over the given repos.
func DefaultSettings(repos []string) Settings {
	if repos == nil {
		repos = []string{}
	}
	return Settings{At: DefaultAt, Repos: repos, TokenAgeDays: DefaultTokenAgeDays}
}

// Load reads the state file.
func (h *Heimdall) Load() error {
	if h.StatePath == "" {
		return nil
	}
	text, err := os.ReadFile(h.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state State
	if err := json.Unmarshal(text, &state); err != nil {
		return fmt.Errorf("%s: %w", h.StatePath, err)
	}
	if state.Findings == nil {
		state.Findings = []Finding{}
	}
	if state.Checks == nil {
		state.Checks = []CheckStatus{}
	}
	if state.Memory == nil {
		state.Memory = map[string]Memory{}
	}
	if state.Raised == nil {
		state.Raised = map[string]string{}
	}
	h.mu.Lock()
	h.state = state
	if state.CheckedAt != nil {
		h.lastDay = state.CheckedAt.In(h.Zone).Format(time.DateOnly)
	}
	h.mu.Unlock()
	return nil
}

// save writes the state file, aside and renamed, readable by you only.
func (h *Heimdall) save() {
	if h.StatePath == "" {
		return
	}
	h.mu.Lock()
	text, err := json.MarshalIndent(h.state, "", "  ")
	h.mu.Unlock()
	if err == nil {
		err = os.MkdirAll(filepath.Dir(h.StatePath), 0o700)
	}
	if err == nil {
		temp := h.StatePath + ".tmp"
		if err = os.WriteFile(temp, text, 0o600); err == nil {
			err = os.Rename(temp, h.StatePath)
		}
	}
	if err != nil {
		h.Logf("keeping the state: %v", err)
	}
}

// Settings is what is set now.
func (h *Heimdall) Settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settingsLocked()
}

func (h *Heimdall) settingsLocked() Settings {
	settings := h.Defaults
	if h.state.Settings != nil {
		settings = *h.state.Settings
	}
	settings.Repos = append([]string{}, settings.Repos...)
	return settings
}

// Mount adds the routes and the nightly loop.
func (h *Heimdall) Mount(ctx context.Context, p *part.Part) {
	p.Handle("GET /v1/report", h.getReport)
	p.Handle("POST /v1/report/run", owner(func(w http.ResponseWriter, _ *http.Request) {
		h.Start(ctx)
		part.JSON(w, http.StatusAccepted, map[string]bool{"running": true})
	}))
	p.Handle("GET /v1/settings", func(w http.ResponseWriter, _ *http.Request) {
		part.JSON(w, http.StatusOK, h.Settings())
	})
	p.Handle("PATCH /v1/settings", owner(h.patchSettings))
	p.Loop("nightly", time.Minute, func(context.Context) error {
		h.Tick(ctx)
		return nil
	})
}

func owner(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(w, http.StatusForbidden, "only the owner may do this")
			return
		}
		handler(w, r)
	}
}

// Report is the findings in one state: active (the default) or resolved.
func (h *Heimdall) Report(resolved bool) Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	report := Report{Running: h.running, Findings: []Finding{}, Checks: []CheckStatus{}}
	if h.state.CheckedAt != nil {
		at := *h.state.CheckedAt
		report.CheckedAt = &at
	}
	for _, f := range h.state.Findings {
		if (f.ResolvedAt != nil) == resolved {
			report.Findings = append(report.Findings, f)
		}
	}
	sortFindings(report.Findings)
	known := map[string]CheckStatus{}
	for _, status := range h.state.Checks {
		known[status.Name] = status
	}
	for _, check := range h.Checks {
		status, ok := known[check.Name()]
		if !ok {
			status = CheckStatus{Name: check.Name(), OK: true}
		}
		report.Checks = append(report.Checks, status)
	}
	return report
}

var severityRank = map[string]int{High: 0, Medium: 1, Low: 2}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if a.Check != b.Check {
			return a.Check < b.Check
		}
		return a.ID < b.ID
	})
}

func (h *Heimdall) getReport(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("state") {
	case "", "active":
		part.JSON(w, http.StatusOK, h.Report(false))
	case "resolved":
		part.JSON(w, http.StatusOK, h.Report(true))
	default:
		part.Error(w, http.StatusBadRequest, "state is active or resolved")
	}
}

var clock = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (h *Heimdall) patchSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		At           *string   `json:"at"`
		Repos        *[]string `json:"repos"`
		TokenAgeDays *int      `json:"tokenAgeDays"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.At != nil && !clock.MatchString(*body.At) {
		part.Error(w, http.StatusBadRequest, "at is HH:MM, e.g. 03:00")
		return
	}
	if body.TokenAgeDays != nil && (*body.TokenAgeDays < 1 || *body.TokenAgeDays > 3650) {
		part.Error(w, http.StatusBadRequest, "tokenAgeDays is 1 to 3650")
		return
	}
	var repos []string
	if body.Repos != nil {
		if len(*body.Repos) > MaxRepos {
			part.Error(w, http.StatusBadRequest, fmt.Sprintf("at most %d repos", MaxRepos))
			return
		}
		repos = []string{}
		for _, path := range *body.Repos {
			path = strings.TrimSpace(path)
			if !filepath.IsAbs(path) {
				part.Error(w, http.StatusBadRequest, "each repo is an absolute path")
				return
			}
			if path = filepath.Clean(path); !slices.Contains(repos, path) {
				repos = append(repos, path)
			}
		}
	}
	h.mu.Lock()
	settings := h.settingsLocked()
	if body.At != nil {
		settings.At = *body.At
		h.lastDay = "" // a new time may fall later today
	}
	if body.TokenAgeDays != nil {
		settings.TokenAgeDays = *body.TokenAgeDays
	}
	if repos != nil {
		settings.Repos = repos
	}
	h.state.Settings = &settings
	h.mu.Unlock()
	h.save()
	part.JSON(w, http.StatusOK, settings)
}

// Tick starts the nightly run once a day, at or after the set time. A
// machine that was off then runs it when it wakes.
func (h *Heimdall) Tick(ctx context.Context) {
	now := h.now().In(h.Zone)
	day := now.Format(time.DateOnly)
	h.mu.Lock()
	at := h.settingsLocked().At
	due := h.lastDay != day && now.Format("15:04") >= at
	h.mu.Unlock()
	if due && h.Start(ctx) {
		h.mu.Lock()
		h.lastDay = day
		h.mu.Unlock()
	}
}

// Start runs every check in the background, unless a run is going on.
// It answers whether it started one.
func (h *Heimdall) Start(ctx context.Context) bool {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return false
	}
	h.running = true
	h.mu.Unlock()
	h.runs.Add(1)
	go func() {
		defer h.runs.Done()
		h.Run(ctx)
	}()
	return true
}

// Wait waits for a background run (tests).
func (h *Heimdall) Wait() { h.runs.Wait() }

// Run runs every check now, merges what they found, and tells Gjallar.
func (h *Heimdall) Run(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()
	h.mu.Lock()
	h.running = true
	settings := h.settingsLocked()
	h.mu.Unlock()

	repos := h.repos(ctx, settings.Repos)
	for _, check := range h.Checks {
		name := check.Name()
		h.mu.Lock()
		env := &Env{
			Repos:    repos,
			Now:      h.now(),
			TokenAge: time.Duration(settings.TokenAgeDays) * 24 * time.Hour,
			Memory:   cloneMemory(h.state.Memory[name]),
		}
		for _, f := range h.state.Findings {
			if f.Check == name && f.ResolvedAt == nil {
				env.Previous = append(env.Previous, f)
			}
		}
		h.mu.Unlock()
		result, err := safeRun(ctx, check, env)
		ran := h.now()
		status := CheckStatus{Name: name, OK: err == nil, LastRun: &ran, Skipped: result.Skipped}
		if err != nil {
			status.Error = err.Error()
			h.Logf("check %s: %v", name, err)
		}
		h.mu.Lock()
		h.setStatus(status)
		if err == nil && result.Skipped == "" {
			h.state.Memory[name] = env.Memory
			h.merge(name, result.Findings, ran)
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	at := h.now()
	h.state.CheckedAt = &at
	h.prune(at)
	h.mu.Unlock()
	h.save()
	h.syncAlerts(ctx)
	h.save()
}

func safeRun(ctx context.Context, check Check, env *Env) (result Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return check.Run(ctx, env)
}

func cloneMemory(m Memory) Memory {
	c := Memory{}
	for k, v := range m {
		c[k] = append([]string{}, v...)
	}
	return c
}

func (h *Heimdall) setStatus(status CheckStatus) {
	for i, s := range h.state.Checks {
		if s.Name == status.Name {
			h.state.Checks[i] = status
			return
		}
	}
	h.state.Checks = append(h.state.Checks, status)
}

// merge folds a check's findings into what is kept: new ones are added, ones
// found again are updated, and active ones not found any more are resolved.
func (h *Heimdall) merge(check string, found []Finding, now time.Time) {
	seen := map[string]bool{}
	for _, f := range found {
		f.Check = check
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		index := slices.IndexFunc(h.state.Findings, func(old Finding) bool { return old.Check == check && old.ID == f.ID })
		if index < 0 {
			f.FirstSeen, f.LastSeen, f.ResolvedAt = now, now, nil
			h.state.Findings = append(h.state.Findings, f)
			continue
		}
		old := h.state.Findings[index]
		f.FirstSeen, f.LastSeen, f.ResolvedAt = old.FirstSeen, now, nil
		if old.ResolvedAt != nil {
			f.FirstSeen = now // back again: a new occurrence
		}
		h.state.Findings[index] = f
	}
	for i, f := range h.state.Findings {
		if f.Check == check && f.ResolvedAt == nil && !seen[f.ID] {
			at := now
			h.state.Findings[i].ResolvedAt = &at
		}
	}
}

// prune drops resolved findings older than KeepResolved.
func (h *Heimdall) prune(now time.Time) {
	kept := h.state.Findings[:0]
	for _, f := range h.state.Findings {
		if f.ResolvedAt == nil || now.Sub(*f.ResolvedAt) < KeepResolved {
			kept = append(kept, f)
		}
	}
	h.state.Findings = kept
}

// repos are the configured repos and Sindri's projects, each once, those
// that are git repos.
func (h *Heimdall) repos(ctx context.Context, paths []string) []Repo {
	var repos []Repo
	add := func(path string) {
		path = filepath.Clean(path)
		if slices.ContainsFunc(repos, func(r Repo) bool { return r.Path == path }) {
			return
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			return
		}
		repos = append(repos, Repo{Name: filepath.Base(path), Path: path})
	}
	for _, path := range paths {
		add(path)
	}
	if h.MoreRepos != nil {
		more, err := h.MoreRepos(ctx)
		if err != nil {
			h.Logf("Sindri's projects: %v", err)
		}
		for _, repo := range more {
			add(repo.Path)
		}
	}
	return repos
}

// syncAlerts raises every high finding on Gjallar and resolves what it
// raised before that is gone. The first time, it takes back the alerts
// Gjallar still holds from before a restart, so they are resolved too.
func (h *Heimdall) syncAlerts(ctx context.Context) {
	if h.Gjallar == nil || h.Gjallar.URL == "" {
		return
	}
	if !h.adopted {
		if held, err := h.Gjallar.Held(ctx); err == nil {
			h.mu.Lock()
			for _, raise := range held {
				if _, known := h.state.Raised[raise.Key]; !known {
					h.state.Raised[raise.Key] = string(raise.Severity)
				}
			}
			h.mu.Unlock()
			h.adopted = true
		}
	}
	h.mu.Lock()
	want := map[string]alert.Raise{}
	for _, f := range h.state.Findings {
		if f.ResolvedAt != nil || f.Severity != High {
			continue
		}
		severity := alert.Warning
		if f.Critical {
			severity = alert.Critical
		}
		want[f.AlertKey()] = alert.Raise{
			Key: clip(f.AlertKey(), 200), Severity: severity,
			Title:  clip("Heimdall: "+f.Title, 200),
			Detail: clip(alertDetail(f), 4000),
		}
	}
	raised := map[string]string{}
	for k, v := range h.state.Raised {
		raised[k] = v
	}
	h.mu.Unlock()
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raise := want[key]
		if err := h.Gjallar.Raise(ctx, raise); err != nil {
			h.Logf("raising %s on Gjallar: %v", key, err)
			continue
		}
		raised[raise.Key] = string(raise.Severity)
	}
	for key := range raised {
		if _, still := want[key]; still {
			continue
		}
		if err := h.Gjallar.Resolve(ctx, key); err != nil {
			h.Logf("resolving %s on Gjallar: %v", key, err)
			continue
		}
		delete(raised, key)
	}
	h.mu.Lock()
	h.state.Raised = raised
	h.mu.Unlock()
}

func alertDetail(f Finding) string {
	lines := []string{f.Detail}
	if f.Repo != "" {
		lines = append(lines, "Repo: "+f.Repo)
	}
	if f.Where != "" {
		lines = append(lines, "Where: "+f.Where)
	}
	if f.Fix != "" {
		lines = append(lines, "Fix: "+f.Fix)
	}
	return strings.Join(lines, "\n")
}

func clip(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-1]) + "…"
}

// StableID is a readable id from its parts, shortened with a hash when long,
// so an alert key stays within Gjallar's 200 characters.
func StableID(parts ...string) string {
	id := strings.Join(parts, ":")
	if len(id) <= 150 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return id[:130] + "~" + hex.EncodeToString(sum[:])[:12]
}
