package norn

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
	"yggdrasil/internal/linear"
	"yggdrasil/internal/mcp"

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
//	OLLAMA_URL       Ollama (http://127.0.0.1:11434); off: no Ollama at all, so no fallback, no Ollama judge and no embeddings (memory recall by recency)
//	CHAT_URL         an OpenAI-compatible server asked first (XeStrata, http://127.0.0.1:8095/v1); off: Ollama only
//	OLLAMA_FALLBACK  off: never ask Ollama when XeStrata fails; the call fails instead (on). Embeddings still use Ollama
//	EMBED_MODEL      for memory recall (nomic-embed-text)
//	MEMORY_LEARN     false keeps memory read-only (true)
//	LAYA_URL, LAYA_KEY  Laya, for the judge, the tone badge and /v1/systemone
//	TIME_ZONE        yours (Europe/Stockholm)
//	SUMMARY_MODEL    phrases the inbox summary (llama3.2:1b); ASK_MODEL answers (the summary model)
//	DISPATCH_MODEL   decides what each message is (qwen2.5:7b); off: the older rules
//	DISPATCH_KEEP_ALIVE  how long Ollama keeps it loaded after a call (unset: Ollama's own 5m)
//	WARM_EVERY       how long XeStrata may sit idle before Norn asks it for a token (2m); off: never
//	YGG_SINDRI_URL   Sindri, for coding and questions that need tools; SINDRI_TOKEN is Norn's token there (unset: agents off)
//	INBOX_JUDGE      laya, typesafe, xestrata, ollama or off (laya; when Laya doesn't answer, Ollama, or XeStrata with Ollama off); INBOX_JUDGE_MODEL, TYPESAFE_KEY
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
	ollamaURL := env.String("OLLAMA_URL", "http://127.0.0.1:11434")
	if ollamaURL == "off" {
		ollamaURL = ""
	}
	client := &llm.Client{
		PrimaryURL: chatURL,
		OllamaURL:  ollamaURL,
		NoFallback: chatURL != "" && env.String("OLLAMA_FALLBACK", "on") == "off",
		Warn:       p.Logf,
		Record: func(call llm.Call) {
			if err := store.AddCall(context.Background(), call); err != nil {
				p.Logf("could not keep a model call: %v", err)
			}
		},
	}
	n := New(Config{
		ProfilePath:       env.String("PROFILE_PATH", "profile/profile.md"),
		EmbedModel:        env.String("EMBED_MODEL", "nomic-embed-text"),
		Learn:             env.String("MEMORY_LEARN", "true") == "true",
		LayaURL:           env.String("LAYA_URL", "http://127.0.0.1:8000"),
		LayaKey:           env.String("LAYA_KEY", ""),
		TimeZone:          env.String("TIME_ZONE", "Europe/Stockholm"),
		SummaryModel:      env.String("SUMMARY_MODEL", "llama3.2:1b"),
		AskModel:          env.String("ASK_MODEL", env.String("SUMMARY_MODEL", "llama3.2:1b")),
		DispatchModel:     env.String("DISPATCH_MODEL", "qwen2.5:7b"),
		DispatchKeepAlive: env.String("DISPATCH_KEEP_ALIVE", ""),
		Judge: JudgeSettings{
			Provider:    env.String("INBOX_JUDGE", "laya"),
			LayaURL:     env.String("LAYA_URL", "http://127.0.0.1:8000"),
			LayaKey:     env.String("LAYA_KEY", ""),
			TypesafeKey: env.String("TYPESAFE_KEY", ""),
			OllamaURL:   ollamaURL,
			OllamaModel: env.String("INBOX_JUDGE_MODEL", ""),
		},
		DataDir: filepath.Dir(dbPath),
		// Your own Google "Desktop app" OAuth client, for Gmail and Google Calendar.
		GoogleClientID:     env.String("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env.String("GOOGLE_CLIENT_SECRET", ""),
		LayaShadowURL:      env.String("LAYA_SHADOW_URL", "http://127.0.0.1:8000"),
		LayaShadowKey:      env.String("LAYA_SHADOW_KEY", ""),
		DashboardURL:  env.String("DASHBOARD_URL", "http://localhost:17420"),
		Write: WriteConfig{
			LanguageToolURL: env.String("LANGUAGETOOL_URL", "http://127.0.0.1:8010"),
			Model:           env.String("WRITE_MODEL", "qwen2.5:7b"),
			RewriteModel:    env.String("REWRITE_MODEL", "sonnet"),
		},
	}, store, client, p.Logf)
	if chatURL != "" {
		n.Config.Judge.Chat = client.Chat
	}
	if ollamaURL == "" {
		// Embeddings come from Ollama only: without it, memory recall is by recency.
		n.Brain.Embed = nil
	}
	n.Gjallar = alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", ""))
	if token := env.String("SINDRI_TOKEN", ""); token != "" {
		n.Sindri.URL, n.Sindri.Token = where.URL("sindri"), token
	}
	n.Linear = &linear.Client{Key: env.String("LINEAR_KEY", "")}
	// Read-only lookups through your MCP servers, before an agent. LOOKUP=off turns them off.
	if key := env.String("LINEAR_KEY", ""); key != "" && env.String("LOOKUP", "auto") != "off" {
		n.Lookup = &Lookup{Chat: client.ToolChat, Model: env.String("LOOKUP_MODEL", "lookup"), Zone: n.Zone, Servers: []LookupServer{
			{Name: "linear", Client: &mcp.Client{URL: env.String("LINEAR_MCP_URL", "https://mcp.linear.app/mcp"), Token: key}},
		}}
	}
	n.WakeShadow()
	// Ask XeStrata for a token when it has sat idle, so your next message
	// does not wait for Windows to page its model back in (llm.KeepWarm).
	if warm := env.Duration("WARM_EVERY", 2*time.Minute); env.String("WARM_EVERY", "") != "off" {
		p.Loop("keep-warm", warm/2, func(ctx context.Context) error { return client.KeepWarm(ctx, warm/2) })
	}
	n.Mount(p)
	return p.Run(ctx, where.Addr(Name))
}
