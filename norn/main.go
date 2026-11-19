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
//	EMBED_URL        an OpenAI-compatible embedding server, ending in /v1 (the compose `embeddings` service, http://127.0.0.1:8096/v1); off: Ollama makes them
//	EMBED_MODEL      for memory recall (nomic-embed-text-v1.5 on EMBED_URL, nomic-embed-text on Ollama)
//	MEMORY_LEARN     false keeps memory read-only (true)
//	DRAFTS           false stops drafting replies to the inbox by itself (true)
//	LAYA_URL, LAYA_KEY  Laya, for the judge, the tone badge and /v1/systemone
//	TIME_ZONE        yours (Europe/Stockholm)
//	LOCAL_MODEL      one model for every local call below whose own setting is unset, e.g. qwen3:4b on an Ollama-only machine (unset: each its own default)
//	SUMMARY_MODEL    phrases the inbox summary (llama3.2:1b); ASK_MODEL answers (the summary model)
//	DISPATCH_MODEL   decides what each message is (qwen2.5:7b); off: the older rules
//	BRIEF_MODEL      writes a meeting brief on the local model (qwen2.5:7b)
//	NAMES_MODEL      finds people's names in what goes to Claude, to swap them for placeholders first (qwen2.5:7b)
//	WRITE_MODEL      the writing checks (qwen2.5:7b); LOOKUP_MODEL the Linear lookups (lookup), on Ollama too when CHAT_URL is off
//	DISPATCH_KEEP_ALIVE  how long Ollama keeps it loaded after a call (unset: Ollama's own 5m)
//	WARM_EVERY       how long XeStrata may sit idle before Norn asks it for a token (2m); off: never
//	YGG_SINDRI_URL   Sindri, for coding and questions that need tools; SINDRI_TOKEN is Norn's token there (unset: agents off)
//	INBOX_JUDGE      laya, typesafe, xestrata, ollama or off (laya; when Laya doesn't answer, Ollama, or XeStrata with Ollama off); INBOX_JUDGE_MODEL, TYPESAFE_KEY
//	GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET  your own Google "Desktop app" OAuth client; unset is no Google
//	OBSIDIAN_VAULT   your Obsidian vault's folder, for meeting notes both ways (unset: off); OBSIDIAN_VAULT_NAME is what Obsidian calls it (the folder's name)
//	WORK_HOURS       your working day, Monday to Friday, for the gap a task is offered in (09:00-17:00);
//	                 set it in parts.env, where Ratatoskr reads the same one
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
	embedURL := env.String("EMBED_URL", "http://127.0.0.1:8096/v1")
	embedModel := "nomic-embed-text-v1.5"
	if embedURL == "off" {
		embedURL, embedModel = "", "nomic-embed-text"
	}
	client := &llm.Client{
		PrimaryURL: chatURL,
		OllamaURL:  ollamaURL,
		EmbedURL:   embedURL,
		NoFallback: chatURL != "" && env.String("OLLAMA_FALLBACK", "on") == "off",
		Warn:       p.Logf,
		Record: func(call llm.Call) {
			if err := store.AddCall(context.Background(), call); err != nil {
				p.Logf("could not keep a model call: %v", err)
			}
		},
	}
	// model is a call's model: its own setting, else LOCAL_MODEL, else fallback.
	local := env.String("LOCAL_MODEL", "")
	model := func(name, fallback string) string {
		if local != "" {
			fallback = local
		}
		return env.String(name, fallback)
	}
	n := New(Config{
		ProfilePath:       env.String("PROFILE_PATH", "profile/profile.md"),
		EmbedModel:        env.String("EMBED_MODEL", embedModel),
		Learn:             env.String("MEMORY_LEARN", "true") == "true",
		Drafts:            env.String("DRAFTS", "true") == "true",
		LayaURL:           env.String("LAYA_URL", "http://127.0.0.1:8000"),
		LayaKey:           env.String("LAYA_KEY", ""),
		TimeZone:          env.String("TIME_ZONE", "Europe/Stockholm"),
		SummaryModel:      model("SUMMARY_MODEL", "llama3.2:1b"),
		AskModel:          env.String("ASK_MODEL", model("SUMMARY_MODEL", "llama3.2:1b")),
		DispatchModel:     model("DISPATCH_MODEL", "qwen2.5:7b"),
		NamesModel:        model("NAMES_MODEL", "qwen2.5:7b"),
		BriefModel:        model("BRIEF_MODEL", "qwen2.5:7b"),
		DispatchKeepAlive: env.String("DISPATCH_KEEP_ALIVE", ""),
		Judge: JudgeSettings{
			Provider:    env.String("INBOX_JUDGE", "laya"),
			LayaURL:     env.String("LAYA_URL", "http://127.0.0.1:8000"),
			LayaKey:     env.String("LAYA_KEY", ""),
			TypesafeKey: env.String("TYPESAFE_KEY", ""),
			OllamaURL:   ollamaURL,
			OllamaModel: model("INBOX_JUDGE_MODEL", ""),
		},
		DataDir: filepath.Dir(dbPath),
		// Your own Google "Desktop app" OAuth client, for Gmail and Google Calendar.
		GoogleClientID:     env.String("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env.String("GOOGLE_CLIENT_SECRET", ""),
		LayaShadowURL:      env.String("LAYA_SHADOW_URL", "http://127.0.0.1:8000"),
		LayaShadowKey:      env.String("LAYA_SHADOW_KEY", ""),
		DashboardURL:       env.String("DASHBOARD_URL", "http://localhost:17420"),
		WorkHours:          env.String("WORK_HOURS", DefaultWorkHours),
		Write: WriteConfig{
			LanguageToolURL: env.String("LANGUAGETOOL_URL", "http://127.0.0.1:8010"),
			Model:           model("WRITE_MODEL", "qwen2.5:7b"),
			RewriteModel:    env.String("REWRITE_MODEL", "sonnet"),
		},
	}, store, client, p.Logf)
	if chatURL != "" {
		n.Config.Judge.Chat = client.Chat
	}
	if !client.CanEmbed() {
		// Nothing makes embeddings: memory recall goes by recency.
		n.Brain.Embed = nil
	} else {
		// After the embedding model changes, old vectors are made again, in the background.
		p.Loop("memory-vectors", 10*time.Minute, n.Brain.Reembed)
	}
	n.Gjallar = alert.NewClient(where.URL("gjallar"), env.String("GJALLAR_TOKEN", ""))
	if token := env.String("SINDRI_TOKEN", ""); token != "" {
		n.Sindri.URL, n.Sindri.Token = where.URL("sindri"), token
	}
	n.Linear = &linear.Client{Key: env.String("LINEAR_KEY", "")}
	n.Vault = OpenVault(env.String("OBSIDIAN_VAULT", ""), env.String("OBSIDIAN_VAULT_NAME", ""))
	if dir := env.String("OBSIDIAN_VAULT", ""); dir != "" && n.Vault == nil {
		p.Logf("OBSIDIAN_VAULT %s is not an Obsidian vault (no .obsidian); meeting notes are off", dir)
	}
	// Read-only lookups through your MCP servers, before an agent. LOOKUP=off turns them off.
	if key := env.String("LINEAR_KEY", ""); key != "" && env.String("LOOKUP", "auto") != "off" {
		n.Lookup = &Lookup{Chat: client.ToolChat, Model: model("LOOKUP_MODEL", "lookup"), Zone: n.Zone, Servers: []LookupServer{
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
