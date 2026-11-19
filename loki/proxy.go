package loki

// Loki the shapeshifter: a proxy in front of a part. Parts find each other
// only through YGG_<PART>_URL, so with that pointed at Loki's proxy port, Loki
// stands where the part is called and passes every call through to the real
// part (its upstream), untouched: method, path, query, headers, body, status
// and response headers, server-sent events streamed as they come, long calls
// left to run. Until a trick bends it, from outside and without the part's
// help:
//
//   - health: /health answers 503 with ok: false (the part's own answer otherwise).
//   - slow:   every call but /health waits delayMs first.
//   - error:  every call but /health answers 500 (or 502), or only paths under a prefix.
//   - drop:   every connection is closed without an answer.
//   - hang:   every call is held unanswered until the trick ends or the caller gives up.
//
// A trick ends at its until, read from the clock on every call. The proxy is
// dev only: while Loki is down, a proxied part is cut off from the others.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hopHeader marks a call Loki's proxy sent on, so a proxy that would reach
// itself (an upstream that points back at Loki) is caught instead of looping.
// Only the upstream sees it; callers never do.
const hopHeader = "X-Loki-Hop"

// ProxyTrick is what a proxy does to its calls until Until.
type ProxyTrick struct {
	Kind    string
	DelayMs int
	Status  int
	Path    string
	Until   time.Time
}

// Proxy stands in front of one part.
type Proxy struct {
	Part     string
	Addr     string
	Upstream *url.URL

	now     func() time.Time
	hop     string
	reverse *httputil.ReverseProxy

	mu    sync.Mutex
	trick *ProxyTrick
	ended chan struct{}
}

type healthTrickKey struct{}

// NewProxy makes a proxy for the part at addr that passes calls to upstream.
// hop is this Loki's own mark, the same on all its proxies.
func NewProxy(name, addr string, upstream *url.URL, hop string) *Proxy {
	p := &Proxy{Part: name, Addr: addr, Upstream: upstream, now: time.Now, hop: hop, ended: make(chan struct{})}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		// Bodies go through as they are: no gzip asked for or unpacked here.
		DisableCompression: true,
	}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			// As the caller sent it: the Host, and any X-Forwarded-* it had.
			r.Out.Host = r.In.Host
			for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if values, ok := r.In.Header[name]; ok {
					r.Out.Header[name] = values
				}
			}
			r.Out.Header.Set(hopHeader, hop)
		},
		Transport: transport,
		// Every write reaches the caller at once: server-sent events stream.
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.Request.Context().Value(healthTrickKey{}) != nil {
				failHealth(response)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the caller gave up
			}
			// The part didn't answer: the caller finds it as unreachable as it is.
			drop(w)
		},
	}
	return p
}

// Set puts a trick on; it ends at its Until.
func (p *Proxy) Set(trick ProxyTrick) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trick = &trick
	close(p.ended)
	p.ended = make(chan struct{})
}

// Clear takes the trick off now: held calls go on through.
func (p *Proxy) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trick = nil
	close(p.ended)
	p.ended = make(chan struct{})
}

// Active is the trick now, or nil; a trick past its until is dropped here.
func (p *Proxy) Active() *ProxyTrick {
	trick, _ := p.active()
	return trick
}

func (p *Proxy) active() (*ProxyTrick, chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trick != nil && !p.now().Before(p.trick.Until) {
		p.trick = nil
	}
	if p.trick == nil {
		return nil, p.ended
	}
	copied := *p.trick
	return &copied, p.ended
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(hopHeader) == p.hop {
		http.Error(w, "Loki's proxy for "+p.Part+" reaches itself: set LOKI_UPSTREAM_"+strings.ToUpper(p.Part)+" to the real part", http.StatusLoopDetected)
		return
	}
	trick, ended := p.active()
	if trick != nil {
		health := r.URL.Path == "/health"
		switch trick.Kind {
		case "health":
			if health {
				r = r.WithContext(context.WithValue(r.Context(), healthTrickKey{}, true))
			}
		case "slow":
			if !health {
				delay := min(time.Duration(trick.DelayMs)*time.Millisecond, trick.Until.Sub(p.now()))
				if !wait(r.Context(), delay, ended) {
					return
				}
			}
		case KindError:
			if !health && strings.HasPrefix(r.URL.Path, trick.Path) {
				status := trick.Status
				if status == 0 {
					status = http.StatusInternalServerError
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"`+strings.ToLower(http.StatusText(status))+`"}`+"\n")
				return
			}
		case KindDrop:
			drop(w)
			return
		case KindHang:
			if !wait(r.Context(), trick.Until.Sub(p.now()), ended) {
				return
			}
		}
	}
	p.reverse.ServeHTTP(w, r)
}

// wait waits for d, or until the trick is taken off; false when the caller gave up.
func wait(ctx context.Context, d time.Duration, ended chan struct{}) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ended:
		return true
	case <-ctx.Done():
		return false
	}
}

// drop closes the caller's connection without a word, with a reset when it can.
func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// failHealth turns the part's own /health answer into a 503 with ok: false.
func failHealth(response *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	var health map[string]any
	if json.Unmarshal(body, &health) != nil || health == nil {
		health = map[string]any{}
	}
	health["ok"] = false
	text, _ := json.Marshal(health)
	text = append(text, '\n')
	response.StatusCode = http.StatusServiceUnavailable
	response.Status = "503 Service Unavailable"
	response.Header.Set("Content-Type", "application/json")
	response.Header.Del("Content-Encoding")
	response.Header.Del("Transfer-Encoding")
	response.Header.Set("Content-Length", strconv.Itoa(len(text)))
	response.ContentLength = int64(len(text))
	response.TransferEncoding = nil
	response.Body = io.NopCloser(bytes.NewReader(text))
}

// Listen opens the proxy's port, so a port already taken is found at start.
func (p *Proxy) Listen() (net.Listener, error) {
	return net.Listen("tcp", p.Addr)
}

// Serve serves on listener until ctx ends. Streams and held calls are cut
// then: the proxy goes with Loki.
func (p *Proxy) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// SameAddress is whether two host:port addresses, or URLs, name the same
// place on this machine: localhost, 127.0.0.1 and an empty or 0.0.0.0 host
// are all loopback.
func SameAddress(a, b string) bool {
	norm := func(s string) string {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			s = u.Host
			if u.Port() == "" {
				switch u.Scheme {
				case "https":
					s += ":443"
				default:
					s += ":80"
				}
			}
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return s
		}
		switch host {
		case "", "localhost", "0.0.0.0", "::", "::1", "127.0.0.1":
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port)
	}
	return norm(a) == norm(b)
}
