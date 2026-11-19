package loki

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// DefaultWatch is Ratatoskr's watch list when RATATOSKR_WATCH is unset; Loki
// reads it only when Ratatoskr doesn't answer.
const DefaultWatch = "norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr,hermodr,loki"

// Main runs Loki until ctx ends. Settings:
//
//	LOKI_ADDR          listen address (127.0.0.1:8780)
//	LOKI_TOKENS        who may call it: owner=…
//	LOKI_STATE         where settings and strikes are kept (data/loki.json)
//	LOKI_PARTS         the parts it may trick (every part but Loki)
//	<PART>_TOKEN       Loki's token at each part (NORN_TOKEN, GJALLAR_TOKEN…), at YGG_<PART>_URL:
//	                   its trick switch, Ratatoskr's status, Gjallar's alerts, Hermóðr's channels
//	GJALLAR_HEARTBEATS the senders Gjallar expects to beat (ratatoskr=3m), as in gjallar.env
//	RATATOSKR_WATCH    the parts Ratatoskr watches, read when Ratatoskr doesn't answer
//	WORK_HOURS         your working day, Monday to Friday (09:00-17:00), for passive strikes
//	TIME_ZONE          yours (Europe/Stockholm)
//	LOKI_PROXY         the parts Loki stands in front of (none): norn,sindri,… (dev only)
//	LOKI_PROXY_ADDR_<PART>  where a part's proxy listens (127.0.0.1:<its port + 100>, Norn 8871)
//	LOKI_UPSTREAM_<PART>    where the real part is (http://127.0.0.1:<its port>), never YGG_<PART>_URL
//
// A part answers Loki's switch tricks only with LOKI=on in its own
// environment. A proxied part is reached through Loki only when YGG_<PART>_URL
// in parts.env points at its proxy; Loki itself calls it at its upstream.
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("LOKI_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("LOKI_TOKENS is empty: set at least owner=<token>")
	}
	parts := env.List("LOKI_PARTS")
	if len(parts) == 0 {
		parts = KnownParts()
	}
	urls := map[string]string{}
	clients := map[string]*call.Part{}
	for _, name := range parts {
		if _, known := where.Ports[name]; !known || name == Name {
			return fmt.Errorf("LOKI_PARTS: %q is not another part", name)
		}
	}
	proxies, err := Proxies(env.List("LOKI_PROXY"), newID())
	if err != nil {
		return err
	}
	for _, name := range KnownParts() {
		urls[name] = where.URL(name)
		if proxy := proxies[name]; proxy != nil {
			// YGG_<PART>_URL points at the proxy: Loki goes around it.
			urls[name] = proxy.Upstream.String()
		}
		clients[name] = call.New(name, urls[name], env.String(strings.ToUpper(name)+"_TOKEN", ""), 15*time.Second)
	}
	l := New(parts, urls, clients, alert.NewClient(urls["gjallar"], env.String("GJALLAR_TOKEN", "")))
	l.Proxies = proxies
	for _, pair := range env.Pairs(env.String("GJALLAR_HEARTBEATS", "ratatoskr=3m")) {
		l.Heartbeats = append(l.Heartbeats, pair.Name)
	}
	l.Watch = env.List("RATATOSKR_WATCH")
	if len(l.Watch) == 0 {
		l.Watch = strings.Split(DefaultWatch, ",")
	}
	zone, err := time.LoadLocation(env.String("TIME_ZONE", "Europe/Stockholm"))
	if err != nil {
		return fmt.Errorf("TIME_ZONE: %w", err)
	}
	l.Hours.Zone = zone
	if l.Hours.From, l.Hours.To, err = ParseHours(env.String("WORK_HOURS", "09:00-17:00")); err != nil {
		return fmt.Errorf("WORK_HOURS: %w", err)
	}
	l.StatePath = env.String("LOKI_STATE", "data/loki.json")
	if err := l.Load(); err != nil {
		return fmt.Errorf("LOKI_STATE: %w", err)
	}
	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	l.Logf = p.Logf
	var missing []string
	for _, name := range parts {
		if !clients[name].On() {
			missing = append(missing, strings.ToUpper(name)+"_TOKEN")
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		p.Logf("no token at some parts, so they can't be tricked or read: %s", strings.Join(missing, ", "))
	}
	for _, name := range KnownParts() {
		proxy := proxies[name]
		if proxy == nil {
			continue
		}
		listener, err := proxy.Listen()
		if err != nil {
			return fmt.Errorf("LOKI_PROXY: %s's proxy: %w", name, err)
		}
		if called := where.URL(name); !SameAddress(called, proxy.Addr) {
			p.Logf("proxying %s on %s to %s, but YGG_%s_URL is %s, so no part calls it through Loki", name, proxy.Addr, proxy.Upstream, strings.ToUpper(name), called)
		} else {
			p.Logf("proxying %s on %s to %s: while Loki is down, the other parts can't reach %s", name, proxy.Addr, proxy.Upstream, Title(name))
		}
		go func() {
			if err := proxy.Serve(ctx, listener); err != nil {
				p.Logf("%s's proxy stopped: %v", name, err)
			}
		}()
	}
	l.Mount(ctx, p)
	defer l.ThawAll()
	return p.Run(ctx, where.Addr(Name))
}

// ProxyOffset is how far above a part's own port its proxy listens.
const ProxyOffset = 100

// Proxies makes a proxy for each part named in LOKI_PROXY: at
// LOKI_PROXY_ADDR_<PART> (127.0.0.1:<port + 100>), passing calls to
// LOKI_UPSTREAM_<PART> (http://127.0.0.1:<port>). Never YGG_<PART>_URL, which
// points at the proxy itself; an upstream at any proxy's address or Loki's own
// is refused.
func Proxies(names []string, hop string) (map[string]*Proxy, error) {
	proxies := map[string]*Proxy{}
	for _, name := range names {
		port, known := where.Ports[name]
		if !known || name == Name {
			return nil, fmt.Errorf("LOKI_PROXY: %q is not another part", name)
		}
		upper := strings.ToUpper(name)
		addr := env.String("LOKI_PROXY_ADDR_"+upper, fmt.Sprintf("127.0.0.1:%d", port+ProxyOffset))
		upstream, err := url.Parse(env.String("LOKI_UPSTREAM_"+upper, fmt.Sprintf("http://127.0.0.1:%d", port)))
		if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
			return nil, fmt.Errorf("LOKI_UPSTREAM_%s: %q is not an http URL", upper, env.String("LOKI_UPSTREAM_"+upper, ""))
		}
		proxies[name] = NewProxy(name, addr, upstream, hop)
	}
	for name, proxy := range proxies {
		upper := strings.ToUpper(name)
		if SameAddress(proxy.Upstream.String(), where.Addr(Name)) {
			return nil, fmt.Errorf("LOKI_UPSTREAM_%s is Loki itself (%s): point it at the real %s", upper, proxy.Upstream, Title(name))
		}
		for other, them := range proxies {
			if SameAddress(proxy.Upstream.String(), them.Addr) {
				return nil, fmt.Errorf("LOKI_UPSTREAM_%s is %s's proxy (%s), so Loki would call itself: point it at the real %s", upper, Title(other), them.Addr, Title(name))
			}
			if other != name && SameAddress(proxy.Addr, them.Addr) {
				return nil, fmt.Errorf("LOKI_PROXY_ADDR_%s: %s and %s would share %s", upper, Title(name), Title(other), proxy.Addr)
			}
		}
	}
	return proxies, nil
}
