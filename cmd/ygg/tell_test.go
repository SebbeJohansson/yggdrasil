package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

// fake serves routes as JSON for one part and points YGG_<PART>_URL at it.
func fake(t *testing.T, part string, routes map[string]any, seen *[]string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		key := r.Method + " " + r.URL.Path
		if seen != nil {
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(r.Body)
			*seen = append(*seen, strings.TrimSpace(key+" "+body.String()))
		}
		answer, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	t.Setenv("YGG_"+strings.ToUpper(part)+"_URL", server.URL)
}

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("YGG_TOKEN", "owner")
	buffer := new(bytes.Buffer)
	out = buffer
	now = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		out = nil
		now = time.Now
	})
	return buffer
}

func TestStatusSaysWhoIsDownAndWhatIsLate(t *testing.T) {
	printed := capture(t)
	fake(t, "ratatoskr", map[string]any{"GET /v1/status": map[string]any{
		"gjallarUp": true,
		"parts": []any{
			map[string]any{"name": "norn", "up": true, "health": map[string]any{"loops": map[string]any{
				"inbox":    map[string]any{"everySeconds": 20, "lastRunAt": fixedNow.Add(-10 * time.Second)},
				"calendar": map[string]any{"everySeconds": 60, "lastRunAt": fixedNow.Add(-12 * time.Minute)},
			}}},
			map[string]any{"name": "munin", "up": false, "error": "connection refused"},
		},
	}}, nil)
	fake(t, "gjallar", map[string]any{"GET /v1/alerts": map[string]any{"alerts": []any{map[string]any{"id": 1}}}}, nil)

	if code := status(nil); code != 1 {
		t.Fatalf("a part is down, so status exits 1, got %d", code)
	}
	text := printed.String()
	for _, want := range []string{"1 of 2 parts up", "✗ munin       down: connection refused", "! norn        up, calendar loop late, last ran 12m ago", "Gjallar: 1 open alert"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestStatusSaysWhenRatatoskrIsNotThere(t *testing.T) {
	capture(t)
	t.Setenv("YGG_RATATOSKR_URL", "http://127.0.0.1:1")
	if code := status(nil); code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
}

func TestPickTrick(t *testing.T) {
	targets := []target{{
		Name: "norn", Up: true,
		Vias: []struct {
			Via       string `json:"via"`
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		}{{Via: "switch", Available: true}, {Via: "proxy", Available: false}},
		Tricks: []trick{
			{Via: "proxy", Kind: "health"},
			{Via: "switch", Kind: "health", Label: "Norn stops answering /health"},
			{Via: "switch", Kind: "loop", Loop: "calendar"},
			{Via: "switch", Kind: "loop", Loop: "inbox"},
		},
	}, {Name: "munin", Up: false}}

	got, err := pickTrick(targets, "norn", "", "")
	if err != nil || got.Via != "switch" || got.Kind != "health" {
		t.Fatalf("health by an open way: %+v %v", got, err)
	}
	if got, _ := pickTrick(targets, "norn", "loop", "inbox"); got.Loop != "inbox" {
		t.Fatalf("the loop asked for: %+v", got)
	}
	if _, err := pickTrick(targets, "norn", "slow", ""); err == nil || !strings.Contains(err.Error(), "health, loop:calendar, loop:inbox") {
		t.Fatalf("says what it can do: %v", err)
	}
	if _, err := pickTrick(targets, "munin", "", ""); err == nil || !strings.Contains(err.Error(), "down already") {
		t.Fatalf("a part that is down: %v", err)
	}
	if _, err := pickTrick(targets, "odin", "", ""); err == nil || !strings.Contains(err.Error(), "knows: norn, munin") {
		t.Fatalf("an unknown part: %v", err)
	}
}

func lokiTargets() map[string]any {
	return map[string]any{
		"GET /v1/targets": map[string]any{"parts": []any{map[string]any{
			"name": "norn", "up": true,
			"vias":   []any{map[string]any{"via": "switch", "available": true}},
			"tricks": []any{map[string]any{"via": "switch", "kind": "health", "label": "Norn stops answering /health"}},
		}}},
		"POST /v1/strikes": map[string]any{"id": "s1", "part": "norn", "state": "striking", "endsAt": fixedNow.Add(3 * time.Minute)},
	}
}

func TestStrikeAsksFirstAndCallsOffOnNo(t *testing.T) {
	printed := capture(t)
	var seen []string
	fake(t, "loki", lokiTargets(), &seen)
	in = strings.NewReader("n\n")
	t.Cleanup(func() { in = nil })

	if code := strike([]string{"norn"}); code != 0 {
		t.Fatalf("got %d", code)
	}
	if !strings.Contains(printed.String(), "Loki will norn stops answering /health for 3 min (switch). Go? [y/N] Called off.") {
		t.Fatalf("asked and called off:\n%s", printed)
	}
	for _, call := range seen {
		if strings.HasPrefix(call, "POST") {
			t.Fatalf("struck anyway: %v", seen)
		}
	}
}

func TestStrikeWithYesStrikes(t *testing.T) {
	printed := capture(t)
	var seen []string
	fake(t, "loki", lokiTargets(), &seen)

	if code := strike([]string{"norn", "--minutes", "2", "--yes"}); code != 0 {
		t.Fatalf("got %d", code)
	}
	last := seen[len(seen)-1]
	if !strings.HasPrefix(last, "POST /v1/strikes") || !strings.Contains(last, `"minutes":2`) || !strings.Contains(last, `"kind":"health"`) {
		t.Fatalf("struck with: %v", seen)
	}
	if !strings.Contains(printed.String(), "Loki strikes norn until") {
		t.Fatalf("said so:\n%s", printed)
	}
}

func TestFindingsLeaveTheSmallOnesForAll(t *testing.T) {
	list := []finding{
		{ID: "a", Severity: "low"},
		{ID: "b", Severity: "high"},
		{ID: "c", Severity: "medium", ResolvedAt: "2026-11-01T00:00:00Z"},
		{ID: "d", Severity: "medium"},
	}
	ids := func(fs []finding) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(openFindings(list, false)); got != "b,d" {
		t.Fatalf("got %s", got)
	}
	if got := ids(openFindings(list, true)); got != "b,d,a" {
		t.Fatalf("got %s", got)
	}
}

func TestLastExchangesKeepEachExchangeTogether(t *testing.T) {
	lines := []gossipLine{{ID: 1, Exchange: "x"}, {ID: 2, Exchange: "x"}, {ID: 3, Exchange: "y"}, {ID: 4, Exchange: "z"}, {ID: 5, Exchange: "z"}}
	got := lastExchanges(lines, 2)
	if len(got) != 2 || got[0][0].Exchange != "y" || len(got[1]) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestFlagsMayComeAfterTheWords(t *testing.T) {
	capture(t)
	var seen []string
	fake(t, "loki", lokiTargets(), &seen)
	if code := strike([]string{"--yes", "norn", "health", "--minutes=4"}); code != 0 {
		t.Fatalf("got %d", code)
	}
	if !strings.Contains(seen[len(seen)-1], `"minutes":4`) {
		t.Fatalf("got %v", seen)
	}
}

func TestLiveAgentsPutsWhatNeedsYouFirst(t *testing.T) {
	got := liveAgents([]sindriAgent{{ID: "1", Status: "done"}, {ID: "2", Status: "working"}, {ID: "3", Status: "needs-approval"}, {ID: "4", Status: "aborted"}, {ID: "5", Status: "waiting"}})
	var order []string
	for _, a := range got {
		order = append(order, a.ID)
	}
	if strings.Join(order, ",") != "3,5,2" {
		t.Fatalf("got %v", order)
	}
}
