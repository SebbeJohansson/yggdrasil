package sindri

import (
	"testing"
	"time"
)

func TestUsageTotalsAndWindow(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	u := &Usage{Store: store, Budget: 1000, Window: time.Hour}
	now := store.ms()
	u.Record(UsageEntry{At: now, Source: "agent", Agent: "a1", GoalID: 7, Model: "sonnet", TokensIn: 300, TokensOut: 100, CostUSD: 0.5})
	u.Record(UsageEntry{At: now, Source: "complete", Caller: "norn", Feature: "judge", Model: "haiku", TokensIn: 200, TokensOut: 50})
	u.Record(UsageEntry{At: now - int64(48*time.Hour/time.Millisecond), Source: "complete", Caller: "norn", TokensIn: 5000})
	r, err := u.Report(now - 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.Calls != 2 || r.Total.TokensIn != 500 || r.Total.TokensOut != 150 {
		t.Fatalf("total %+v", r.Total)
	}
	if len(r.ByGoal) != 2 || len(r.ByCaller) != 2 {
		t.Fatalf("groups %+v %+v", r.ByGoal, r.ByCaller)
	}
	if r.Window.Tokens != 650 || r.Window.Share != 0.65 {
		t.Fatalf("window %+v", r.Window)
	}
}

func TestClaudeTokens(t *testing.T) {
	model, in, out, cost := claudeTokens([]byte(`{"result":"x","total_cost_usd":0.02,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100},"modelUsage":{"claude-haiku-4-5":{}}}`))
	if model != "claude-haiku-4-5" || in != 110 || out != 5 || cost != 0.02 {
		t.Fatal(model, in, out, cost)
	}
}
