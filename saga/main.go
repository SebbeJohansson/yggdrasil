package saga

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/env"
	"yggdrasil/internal/part"
	"yggdrasil/internal/sse"
	"yggdrasil/internal/where"
)

// Main runs Sága until ctx ends. Settings:
//
//	SAGA_ADDR            listen address (127.0.0.1:8782)
//	SAGA_TOKENS          who may call it: owner=… (32+ characters each)
//	SAGA_DB              the SQLite file (~/.local/state/yggdrasil/saga.db)
//	SAGA_DEVICE          this machine's name at Mímir (desktop)
//	MIMIR_URL            Mímir on the VPS, e.g. https://vps.tailnet.ts.net
//	MIMIR_TOKEN          this machine's token there
//	GJALLAR_OWNER_TOKEN  your owner token at Gjallar: Sága reads alerts and acts on them for you
//	SAGA_GJALLAR_TOKENS  a Gjallar sender token per other device, named after it: phone=…
//	YGG_GJALLAR_URL      where Gjallar is
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("SAGA_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("SAGA_TOKENS is empty: set at least owner=<token>")
	}
	devices, err := env.Tokens("SAGA_GJALLAR_TOKENS")
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := env.String("SAGA_DB", filepath.Join(home, ".local/state/yggdrasil/saga.db"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	store, err := OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()

	gjallarURL := where.URL("gjallar")
	g := &Gjallar{
		Owner:   call.New("gjallar", gjallarURL, env.String("GJALLAR_OWNER_TOKEN", ""), 10*time.Second),
		Devices: map[string]*call.Part{},
	}
	for token, device := range devices {
		g.Devices[device] = call.New("gjallar", gjallarURL, token, 10*time.Second)
	}
	mimir := &Mimir{
		URL:   strings.TrimRight(env.String("MIMIR_URL", ""), "/"),
		Token: env.String("MIMIR_TOKEN", ""),
		HTTP:  &http.Client{Timeout: 20 * time.Second},
	}
	s := New(store, mimir, g, env.String("SAGA_DEVICE", "desktop"))

	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	s.Mount(p)

	// Sync soon after anything changes, at either end: the loop is the floor.
	nudges := make(chan struct{}, 1)
	nudge := func(sse.Event) {
		select {
		case nudges <- struct{}{}:
		default:
		}
	}
	go s.Nudge(ctx, nudges)
	started := time.Now()
	go sse.New("gjallar", gjallarURL+"/v1/alerts/stream", g.Owner.Token, started).Follow(ctx, nudge)
	if mimir.on() {
		mark, _, err := store.Mark(ctx, "read")
		if err != nil {
			return err
		}
		stream := sse.New("mimir", mimir.URL+"/v1/stream?"+url.Values{"from": {s.Device}}.Encode(), mimir.Token, started)
		stream.Resume(fmt.Sprint(mark))
		go stream.Follow(ctx, nudge)
	}
	return p.Run(ctx, where.Addr(Name))
}
