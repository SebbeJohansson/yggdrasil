package loki

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// proxied is a part behind Loki's proxy: an upstream that tells what it got.
type proxied struct {
	upstream *httptest.Server
	proxy    *Proxy
	server   *httptest.Server
	release  chan struct{}
}

func newProxied(t *testing.T) *proxied {
	t.Helper()
	p := &proxied{release: make(chan struct{})}
	p.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"part":"norn","version":"dev","loops":{}}`)
		case "/v1/events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-p.release:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: two\n\n")
		default:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Part-Says", "hello")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "body": string(body),
				"authorization": r.Header.Get("Authorization"), "custom": r.Header.Get("X-Custom"), "host": r.Host,
				"forwardedFor": r.Header.Get("X-Forwarded-For"),
			})
		}
	}))
	t.Cleanup(p.upstream.Close)
	target, _ := url.Parse(p.upstream.URL)
	p.proxy = NewProxy("norn", "127.0.0.1:0", target, "hop-test")
	p.server = httptest.NewServer(p.proxy)
	t.Cleanup(p.server.Close)
	return p
}

func (p *proxied) get(t *testing.T, path string, timeout time.Duration) (*http.Response, string, error) {
	t.Helper()
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(p.server.URL + path)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response, string(body), nil
}

func TestTheProxyPassesEverythingThroughUntouched(t *testing.T) {
	p := newProxied(t)
	request, _ := http.NewRequest(http.MethodPatch, p.server.URL+"/v1/things/7?a=1&b=two", strings.NewReader(`{"x":1}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Custom", "kept")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got map[string]string
	_ = json.NewDecoder(response.Body).Decode(&got)
	if response.StatusCode != http.StatusTeapot || response.Header.Get("X-Part-Says") != "hello" {
		t.Errorf("answer: %d %v", response.StatusCode, response.Header)
	}
	want := map[string]string{"method": "PATCH", "path": "/v1/things/7", "query": "a=1&b=two", "body": `{"x":1}`,
		"authorization": "Bearer secret", "custom": "kept", "host": strings.TrimPrefix(p.server.URL, "http://"), "forwardedFor": ""}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: %q, want %q", key, got[key], value)
		}
	}
	for name := range response.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-loki") {
			t.Errorf("the proxy shows itself: %s", name)
		}
	}
}

func TestServerSentEventsStreamThroughTheProxy(t *testing.T) {
	p := newProxied(t)
	response, err := http.Get(p.server.URL + "/v1/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type: %s", response.Header.Get("Content-Type"))
	}
	lines := make(chan string, 10)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if text := scanner.Text(); text != "" {
				lines <- text
			}
		}
		close(lines)
	}()
	// The first event arrives while the part still holds the stream open.
	select {
	case line := <-lines:
		if line != "data: one" {
			t.Fatalf("first: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event was held back: the proxy buffers")
	}
	time.Sleep(100 * time.Millisecond)
	close(p.release)
	select {
	case line := <-lines:
		if line != "data: two" {
			t.Fatalf("second: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second event never came")
	}
}

func TestProxyTricksAndTheirEnd(t *testing.T) {
	p := newProxied(t)
	soon := func(d time.Duration) time.Time { return time.Now().Add(d) }

	t.Run("health", func(t *testing.T) {
		p.proxy.Set(ProxyTrick{Kind: "health", Until: soon(300 * time.Millisecond)})
		response, body, err := p.get(t, "/health", time.Second)
		if err != nil || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, `"part":"norn"`) {
			t.Fatalf("tricked /health: %v %v %s", err, response, body)
		}
		if response, _, _ := p.get(t, "/v1/x", time.Second); response.StatusCode != http.StatusTeapot {
			t.Errorf("a route under a health trick: %d", response.StatusCode)
		}
		time.Sleep(350 * time.Millisecond)
		if response, _, _ := p.get(t, "/health", time.Second); response.StatusCode != http.StatusOK {
			t.Errorf("after until: %d", response.StatusCode)
		}
	})

	t.Run("slow", func(t *testing.T) {
		p.proxy.Set(ProxyTrick{Kind: "slow", DelayMs: 250, Until: soon(5 * time.Second)})
		start := time.Now()
		if response, _, _ := p.get(t, "/v1/x", 2*time.Second); response.StatusCode != http.StatusTeapot || time.Since(start) < 250*time.Millisecond {
			t.Errorf("slow route: %d after %s", response.StatusCode, time.Since(start))
		}
		start = time.Now()
		if _, _, err := p.get(t, "/health", time.Second); err != nil || time.Since(start) > 200*time.Millisecond {
			t.Errorf("/health was slowed: %v %s", err, time.Since(start))
		}
		p.proxy.Clear()
	})

	t.Run("error", func(t *testing.T) {
		p.proxy.Set(ProxyTrick{Kind: KindError, Status: 502, Path: "/v1/a", Until: soon(5 * time.Second)})
		if response, body, _ := p.get(t, "/v1/a/b", time.Second); response.StatusCode != http.StatusBadGateway || !strings.Contains(body, "error") {
			t.Errorf("under the path: %d %s", response.StatusCode, body)
		}
		if response, _, _ := p.get(t, "/v1/c", time.Second); response.StatusCode != http.StatusTeapot {
			t.Errorf("outside the path: %d", response.StatusCode)
		}
		if response, _, _ := p.get(t, "/health", time.Second); response.StatusCode != http.StatusOK {
			t.Errorf("/health under an error trick: %d", response.StatusCode)
		}
		p.proxy.Set(ProxyTrick{Kind: KindError, Until: soon(5 * time.Second)})
		if response, _, _ := p.get(t, "/v1/c", time.Second); response.StatusCode != http.StatusInternalServerError {
			t.Errorf("every route: %d", response.StatusCode)
		}
		p.proxy.Clear()
	})

	t.Run("drop", func(t *testing.T) {
		p.proxy.Set(ProxyTrick{Kind: KindDrop, Until: soon(300 * time.Millisecond)})
		if response, _, err := p.get(t, "/health", time.Second); err == nil {
			t.Errorf("dropped, yet answered %d", response.StatusCode)
		}
		time.Sleep(350 * time.Millisecond)
		if response, _, err := p.get(t, "/health", time.Second); err != nil || response.StatusCode != http.StatusOK {
			t.Errorf("after until: %v", err)
		}
	})

	t.Run("hang", func(t *testing.T) {
		p.proxy.Set(ProxyTrick{Kind: KindHang, Until: soon(5 * time.Second)})
		if _, _, err := p.get(t, "/health", 200*time.Millisecond); err == nil {
			t.Error("a hung call was answered")
		}
		// A held call goes on through when the trick ends early…
		done := make(chan int, 1)
		go func() {
			response, _, err := p.get(t, "/v1/x", 3*time.Second)
			if err != nil {
				done <- 0
				return
			}
			done <- response.StatusCode
		}()
		time.Sleep(150 * time.Millisecond)
		p.proxy.Clear()
		if code := <-done; code != http.StatusTeapot {
			t.Errorf("released call: %d", code)
		}
		// …or at its until.
		p.proxy.Set(ProxyTrick{Kind: KindHang, Until: soon(300 * time.Millisecond)})
		start := time.Now()
		if response, _, err := p.get(t, "/health", 2*time.Second); err != nil || response.StatusCode != http.StatusOK || time.Since(start) < 250*time.Millisecond {
			t.Errorf("held to until: %v after %s", err, time.Since(start))
		}
	})
}

func TestTheProxyNeverReachesItself(t *testing.T) {
	var proxy *Proxy
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxy.ServeHTTP(w, r) }))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	proxy = NewProxy("norn", strings.TrimPrefix(server.URL, "http://"), target, "hop-self")
	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusLoopDetected {
		t.Errorf("a proxy pointed at itself answered %d", response.StatusCode)
	}

	t.Setenv("LOKI_UPSTREAM_NORN", "http://localhost:8871")
	if _, err := Proxies([]string{"norn"}, "x"); err == nil || !strings.Contains(err.Error(), "call itself") {
		t.Errorf("upstream at its own proxy: %v", err)
	}
	t.Setenv("LOKI_UPSTREAM_NORN", "http://127.0.0.1:8872")
	if _, err := Proxies([]string{"norn", "sindri"}, "x"); err == nil {
		t.Error("upstream at another proxy")
	}
	t.Setenv("LOKI_UPSTREAM_NORN", "http://127.0.0.1:8780")
	if _, err := Proxies([]string{"norn"}, "x"); err == nil || !strings.Contains(err.Error(), "Loki itself") {
		t.Errorf("upstream at Loki: %v", err)
	}
	t.Setenv("LOKI_UPSTREAM_NORN", "")
	if _, err := Proxies([]string{"loki"}, "x"); err == nil {
		t.Error("Loki proxies itself")
	}
	proxies, err := Proxies([]string{"norn", "hermodr"}, "x")
	if err != nil || proxies["norn"].Addr != "127.0.0.1:8871" || proxies["norn"].Upstream.String() != "http://127.0.0.1:8771" || proxies["hermodr"].Addr != "127.0.0.1:8879" {
		t.Errorf("defaults: %v %+v", err, proxies)
	}
}
