package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func paths(t *testing.T) Paths {
	t.Helper()
	agent, ygg := t.TempDir(), t.TempDir()
	return Paths{
		AgentDB: filepath.Join(agent, "data", "agent.db"), AgentData: filepath.Join(agent, "data"), RunnerState: filepath.Join(agent, "runner", "agents.json"),
		NornDB: filepath.Join(ygg, "data", "norn.db"), NornData: filepath.Join(ygg, "data"),
		SindriDB: filepath.Join(ygg, "state", "sindri.db"), SindriState: filepath.Join(ygg, "state", "sindri-agents.json"),
	}
}

// seed writes a my-agent with one of everything.
func seed(t *testing.T, p Paths) {
	t.Helper()
	db, err := open(p.AgentDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scheduled := `{"prompt":"fix it","label":"demo: Fix it","permissionMode":"auto","allowedTools":["Bash(git add:*)"],"slug":"fix-it-1a2b3c","nextTryAt":5}`
	for _, statement := range []string{agentDBSchema,
		`INSERT INTO turns (session, role, content, at, author, label, read_at) VALUES ('s1', 'user', 'fix the titles', 1, NULL, 'Norn', NULL), ('s1', 'assistant', 'Done. Merge?', 2, 'a1', 'Fix titles', NULL)`,
		`INSERT INTO memories (text, source, embedding, model, at) VALUES ('My manager is Ada.', 'manual', '[1,0]', 'nomic', 3)`,
		`INSERT INTO decisions (at, session, message, prompt, source, decision) VALUES (4, 's1', 'fix the titles', 'p', 'model', '{"action":"code"}')`,
		`INSERT INTO inbox_mutes VALUES ('chat-1', 'Lunch', 5)`,
		`INSERT INTO app_settings VALUES ('widget-visibility', '{"place":"away","hideAtHome":[],"hideAway":["gmail"]}'), ('coding-agent', '{"primary":"copilot"}')`,
		`INSERT INTO session_communication VALUES ('s1', 'concise')`,
		`INSERT INTO agent_sessions (agent, session, seen, at, notes_seen) VALUES ('a1', 's1', 'waiting||12', 6, 2)`,
		`INSERT INTO goals (at, title, repos, source, session, context) VALUES (7, 'Dark mode', '["demo"]', 'manual', 'goal-1', '')`,
		`INSERT INTO goal_images (goal_id, at, name, media_type, kind, data) VALUES (1, 8, 'a.png', 'image/png', 'design', x'89504e47')`,
		`INSERT INTO tasks (at, session, goal, kind, project, branch, agent, outcome, goal_id) VALUES (9, 's1', 'fix the titles', 'code', 'demo', 'agent/fix', 'a1', 'asked', 1)`,
		`INSERT INTO tasks (at, session, goal, kind, project, agent, outcome, scheduled) VALUES (10, 's1', 'fix it', 'code', 'demo', 'a2', 'scheduled', '` + scheduled + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	os.WriteFile(filepath.Join(p.AgentData, "google.json"), []byte(`{"refreshToken":"r","scope":"x"}`), 0o600)
	os.MkdirAll(filepath.Dir(p.RunnerState), 0o700)
	os.WriteFile(p.RunnerState, []byte(`[{"id":"a1","label":"demo: Fix titles","cwd":"/x","permissionMode":"auto","status":"waiting","pending":[],"startedAt":1,"lastActivity":2,"costUsd":0,"numTurns":1,"log":[]}]`), 0o600)
	os.MkdirAll(filepath.Join(filepath.Dir(p.RunnerState), "screenshots", "a1"), 0o700)
	os.WriteFile(filepath.Join(filepath.Dir(p.RunnerState), "screenshots", "a1", "view.png"), []byte("png"), 0o600)
}

func count(t *testing.T, path, query string) int {
	t.Helper()
	db, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestEverythingMovesToYggdrasilAndBack(t *testing.T) {
	p := paths(t)
	seed(t, p)
	report, err := FromMyAgent(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM turns WHERE author = 'a1'`:                             1,
		`SELECT count(*) FROM memories WHERE embedding = '[1,0]'`:                    1,
		`SELECT count(*) FROM decisions`:                                             1,
		`SELECT count(*) FROM inbox_mutes`:                                           1,
		`SELECT count(*) FROM agent_sessions WHERE notes_seen = 2`:                   1,
		`SELECT count(*) FROM app_settings WHERE key = 'widget-visibility'`:          1,
		`SELECT count(*) FROM app_settings WHERE key = 'coding-agent'`:               0,
		`SELECT count(*) FROM session_communication WHERE communication = 'concise'`: 1,
	} {
		if got := count(t, p.NornDB, query); got != want {
			t.Errorf("norn: %s = %d, want %d", query, got, want)
		}
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM tasks`:                               2,
		`SELECT count(*) FROM goals`:                               1,
		`SELECT count(*) FROM goal_images WHERE length(data) = 4`:  1,
		`SELECT count(*) FROM settings WHERE key = 'coding-agent'`: 1,
		`SELECT count(*) FROM feed WHERE agent = 'a1' AND communication = 'concise' AND seen = 'waiting||12'`: 1,
	} {
		if got := count(t, p.SindriDB, query); got != want {
			t.Errorf("sindri: %s = %d, want %d", query, got, want)
		}
	}
	// A task waiting for Claude usage now holds what Sindri restarts it with.
	db, _ := open(p.SindriDB)
	var scheduled string
	db.QueryRow(`SELECT scheduled FROM tasks WHERE outcome = 'scheduled'`).Scan(&scheduled)
	db.Close()
	if !strings.Contains(scheduled, `\"cwd\":\"demo\"`) || !strings.Contains(scheduled, `\"slug\":\"fix-it-1a2b3c\"`) || !strings.Contains(scheduled, `"nextTryAt":5`) {
		t.Fatalf("scheduled is %s", scheduled)
	}
	agents, _ := os.ReadFile(p.SindriState)
	var list []map[string]any
	json.Unmarshal(agents, &list)
	if len(list) != 1 || list[0]["session"] != "s1" {
		t.Fatalf("agents: %s", agents)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p.SindriState), "screenshots", "a1", "view.png")); err != nil {
		t.Fatalf("the screenshot did not move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.NornData, "google.json")); err != nil {
		t.Fatalf("the sign-in did not move: %v", err)
	}
	if report.Rows["turns"] != 2 {
		t.Fatalf("report: %+v", report.Rows)
	}

	// Moving again into the same place is refused, and --replace does it.
	if _, err := FromMyAgent(p, Options{}); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("got %v", err)
	}
	if _, err := FromMyAgent(p, Options{Replace: true}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, p.NornDB, `SELECT count(*) FROM turns`); got != 2 {
		t.Fatalf("replace doubled the turns: %d", got)
	}

	// And back into a fresh my-agent.
	back := p
	back.AgentDB, back.AgentData, back.RunnerState = filepath.Join(t.TempDir(), "agent.db"), t.TempDir(), filepath.Join(t.TempDir(), "agents.json")
	if _, err := ToMyAgent(back, Options{}); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM turns`:                                     2,
		`SELECT count(*) FROM memories`:                                  1,
		`SELECT count(*) FROM tasks`:                                     2,
		`SELECT count(*) FROM goals`:                                     1,
		`SELECT count(*) FROM goal_images`:                               1,
		`SELECT count(*) FROM app_settings`:                              2,
		`SELECT count(*) FROM agent_sessions WHERE seen = 'waiting||12'`: 1,
		`SELECT count(*) FROM tasks WHERE scheduled LIKE '%"slug":"fix-it-1a2b3c"%' AND scheduled LIKE '%"prompt":"fix it"%'`: 1,
	} {
		if got := count(t, back.AgentDB, query); got != want {
			t.Errorf("my-agent: %s = %d, want %d", query, got, want)
		}
	}
	if _, err := os.Stat(back.RunnerState); err != nil {
		t.Fatalf("the agents did not go back: %v", err)
	}
}
