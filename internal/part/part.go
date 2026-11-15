// Package part is what every part of Yggdrasil shares: an HTTP server on
// loopback with bearer tokens, a /health that says whether its loops run on
// time (which Ratatoskr reads), and graceful shutdown.
//
// Every route but /health needs a token. Tokens are named (`owner=…,munin=…`),
// and a handler learns who called it, so a part can tell you (owner) from
// another part and refuse what that caller may not do.
package part

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"yggdrasil/api"
)

// Owner is the caller name for you: the widget, the dashboard, the CLI.
const Owner = "owner"

// Version is set at build time (-ldflags "-X yggdrasil/internal/part.Version=…").
var Version = "dev"

// Part is one running service.
type Part struct {
	Name    string
	tokens  map[string]string
	origins []string
	mux     *http.ServeMux
	loops   []*Loop
	started time.Time
	logger  *log.Logger
	routes  []string
}

// New makes a part. tokens maps a token to its caller's name; origins are the
// browser origins allowed to call it (the dashboard).
func New(name string, tokens map[string]string, origins []string) *Part {
	p := &Part{
		Name:    name,
		tokens:  tokens,
		origins: origins,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logger:  log.New(log.Writer(), "["+name+"] ", log.LstdFlags),
	}
	p.mux.HandleFunc("GET /health", p.health)
	p.routes = append(p.routes, "GET /health")
	if spec := api.Spec(name); spec != nil {
		p.mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(spec)
		})
		p.routes = append(p.routes, "GET /openapi.yaml")
	}
	return p
}

// Logf logs with the part's name in front.
func (p *Part) Logf(format string, args ...any) {
	p.logger.Printf(format, args...)
}

type callerKey struct{}

// Caller is who made the request: the name its token was given.
func Caller(r *http.Request) string {
	name, _ := r.Context().Value(callerKey{}).(string)
	return name
}

// Handle adds a route that needs a token. pattern is a net/http pattern,
// e.g. "POST /v1/alerts/{id}/ack".
func (p *Part) Handle(pattern string, handler http.HandlerFunc) {
	p.routes = append(p.routes, pattern)
	p.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		caller, ok := p.authenticate(r)
		if !ok {
			Error(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		handler(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}

func (p *Part) authenticate(r *http.Request) (string, bool) {
	given, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return "", false
	}
	for token, name := range p.tokens {
		if subtle.ConstantTimeCompare([]byte(given), []byte(token)) == 1 {
			return name, true
		}
	}
	return "", false
}

// Routes is every route the part serves, as "METHOD /path" patterns. A test
// holds them against the part's contract.
func (p *Part) Routes() []string {
	return append([]string{}, p.routes...)
}

// ServeHTTP answers CORS for the allowed origins, then routes.
func (p *Part) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && slices.Contains(p.origins, origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	p.mux.ServeHTTP(w, r)
}

// Loop is work a part does on a timer. Its last run is on /health, so
// Ratatoskr can tell a part that answers from one that still does its job.
type Loop struct {
	Name  string
	Every time.Duration
	run   func(context.Context) error

	mu        sync.Mutex
	lastRunAt time.Time
	lastError string
}

// Loop adds work to run now and then every `every` once the part runs.
func (p *Part) Loop(name string, every time.Duration, run func(context.Context) error) *Loop {
	loop := &Loop{Name: name, Every: every, run: run}
	p.loops = append(p.loops, loop)
	return loop
}

func (l *Loop) once(ctx context.Context, logger *log.Logger) {
	err := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
			}
		}()
		return l.run(ctx)
	}()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastRunAt = time.Now()
	l.lastError = ""
	if err != nil && ctx.Err() == nil {
		l.lastError = err.Error()
		logger.Printf("loop %s: %v", l.Name, err)
	}
}

func (l *Loop) start(ctx context.Context, logger *log.Logger, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(l.Every)
		defer ticker.Stop()
		for {
			l.once(ctx, logger)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// LoopHealth is one loop as /health reports it.
type LoopHealth struct {
	EverySeconds int        `json:"everySeconds"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
}

// Health is GET /health's answer.
type Health struct {
	OK        bool                  `json:"ok"`
	Part      string                `json:"part"`
	Version   string                `json:"version"`
	StartedAt time.Time             `json:"startedAt"`
	Loops     map[string]LoopHealth `json:"loops"`
}

func (p *Part) health(w http.ResponseWriter, _ *http.Request) {
	health := Health{OK: true, Part: p.Name, Version: Version, StartedAt: p.started, Loops: map[string]LoopHealth{}}
	for _, loop := range p.loops {
		loop.mu.Lock()
		entry := LoopHealth{EverySeconds: int(loop.Every / time.Second), LastError: loop.lastError}
		if !loop.lastRunAt.IsZero() {
			at := loop.lastRunAt
			entry.LastRunAt = &at
		}
		loop.mu.Unlock()
		health.Loops[loop.Name] = entry
	}
	JSON(w, http.StatusOK, health)
}

// Run serves on addr and runs the loops until ctx ends, then shuts down,
// giving requests and loops up to 10 s to finish.
func (p *Part) Run(ctx context.Context, addr string) error {
	server := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	loopCtx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	var wg sync.WaitGroup
	for _, loop := range p.loops {
		loop.start(loopCtx, p.logger, &wg)
	}
	failed := make(chan error, 1)
	go func() {
		p.Logf("listening on %s", addr)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	var err error
	select {
	case err = <-failed:
	case <-ctx.Done():
	}
	stopLoops()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	wg.Wait()
	return err
}

// JSON writes v with the status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes `{"error": message}`.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]string{"error": message})
}

// MaxBody is the most a request body may be.
const MaxBody = 1 << 20

// Decode reads a JSON body into v, refusing bodies over MaxBody. Fields it
// does not know are ignored, as my-agent did: a client that sends a whole
// object back (the widget's settings) or a few extra keys still works. The
// error is fit to send back as a 400.
func Decode(r *http.Request, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, MaxBody))
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("bad body: %w", err)
	}
	return nil
}
