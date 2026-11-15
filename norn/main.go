package norn

import (
	"context"
	"fmt"
	"path/filepath"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/env"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
)

// Main runs Norn until ctx ends. Settings:
//
//	NORN_ADDR        listen address (127.0.0.1:8771)
//	NORN_TOKENS      who may call it: owner=…,sindri=…
//	NORN_DB          the SQLite file (data/norn.db)
//	PROFILE_PATH     your profile, markdown (profile/profile.md)
//	OLLAMA_URL       Ollama (http://127.0.0.1:11434)
//	CHAT_URL         an OpenAI-compatible server asked first (XeStrata, http://127.0.0.1:8095/v1); off: Ollama only
//	EMBED_MODEL      for memory recall (nomic-embed-text)
//	MEMORY_LEARN     false keeps memory read-only (true)
//	LAYA_URL, LAYA_KEY  Laya, for the judge, the tone badge and /v1/systemone
//	TIME_ZONE        yours (Europe/Stockholm)
//	GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET  your own Google "Desktop app" OAuth client; unset is no Google
//	YGG_GJALLAR_URL  Gjallar; GJALLAR_TOKEN is Norn's token there (a sign-in that ended is raised)
func Main(ctx context.Context) error {
	tokens, err := env.Tokens("NORN_TOKENS")
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("NORN_TOKENS is empty: set at least owner=<token>")
	}
	dbPath := env.String("NORN_DB", "data/norn.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	p := part.New(Name, tokens, env.List("YGG_CORS_ORIGINS"))
	chatURL := env.String("CHAT_URL", "http://127.0.0.1:8095/v1")
	if chatURL == "off" {
		chatURL = ""
	}
	client := &llm.Client{
		PrimaryURL: chatURL,
		OllamaURL:  env.String("OLLAMA_URL", "http://127.0.0.1:11434"),
		Warn:       p.Logf,
		Record: func(call llm.Call) {
			if err := store.AddCall(context.Background(), call); err != nil {
				p.Logf("could not keep a model call: %v", err)
			}
		},
	}
	n := New(Config{
		ProfilePath: env.String("PROFILE_PATH", "profile/profile.md"),
		EmbedModel:  env.String("EMBED_MODEL", "nomic-embed-text"),
		Learn:       env.String("MEMORY_LEARN", "true") == "true",
		LayaURL:     env.String("LAYA_URL", "http://127.0.0.1:8000"),
		LayaKey:     env.String("LAYA_KEY", ""),
		TimeZone:    env.String("TIME_ZONE", "Europe/Stockholm"),
		DataDir:     filepath.Dir(dbPath),
		// Your own Google "Desktop app" OAuth client, for Gmail and Google Calendar.
		GoogleClientID:     env.String("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env.String("GOOGLE_CLIENT_SECRET", ""),
	}, store, client, p.Logf)
	n.Gjallar = alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", ""))
	n.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
