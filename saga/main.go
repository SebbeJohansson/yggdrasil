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
//	SINDRI_TOKEN         Sága's token at Sindri: this machine's coding sessions go to Mímir. Empty leaves them out
//	SAGA_DRIVE_FROM      the devices whose requests to this machine's sessions are carried out (desktop,phone). Empty: look only
//	YGG_SINDRI_URL       where Sindri is
//
// Without GJALLAR_OWNER_TOKEN Sága syncs no alerts: a laptop that only shares
// its coding sessions runs Sindri and Sága, and nothing else.
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
	// Cloud starts wait for the CLI to print the session's link (up to 90 s).
	s.Sindri = call.New("sindri", where.URL("sindri"), env.String("SINDRI_TOKEN", ""), 2*time.Minute)
	s.DriveFrom = env.List("SAGA_DRIVE_FROM")
	if !g.on() && !s.Sindri.On() {
		return fmt.Errorf("Sága has nothing to sync: set GJALLAR_OWNER_TOKEN (alerts), SINDRI_TOKEN (coding sessions), or both")
	}

	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	s.Mount(p)

	// Sync soon after anything changes, at either end: the loop is the floor.
	nudge := func(sse.Event) { s.nudge() }
	go s.Nudge(ctx, s.nudges)
	started := time.Now()
	if g.on() {
		go sse.New("gjallar", gjallarURL+"/v1/alerts/stream", g.Owner.Token, started).Follow(ctx, nudge)
	}
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
