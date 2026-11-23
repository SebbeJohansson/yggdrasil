package sindri

// The Claude usage ledger: every agent turn and every POST /v1/complete call
// is written down with its model, tokens in and out, and who asked (the goal
// for an agent, the caller and feature for a one-shot answer), so you can see
// what used Claude before a limit stops the work. GET /v1/usage answers
// totals by day, goal and caller. When the tokens used in the plan's window
// pass 80% of SINDRI_USAGE_BUDGET, `claude-usage` (warning) is raised in
// Gjallar, and resolved when the share falls again.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
)

const usageSchema = `
CREATE TABLE IF NOT EXISTS usage (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	source TEXT NOT NULL,
	caller TEXT NOT NULL DEFAULT '',
	feature TEXT NOT NULL DEFAULT '',
	agent TEXT NOT NULL DEFAULT '',
	goal_id INTEGER,
	model TEXT NOT NULL DEFAULT '',
	tokens_in INTEGER NOT NULL DEFAULT 0,
	tokens_out INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_by_at ON usage (at);
`

// UsageAlertShare is the share of the budget at which the alert is raised.
const UsageAlertShare = 0.8

// UsageEntry is one turn or one call.
type UsageEntry struct {
	At        int64   `json:"at"`
	Source    string  `json:"source"` // "agent" or "complete"
	Caller    string  `json:"caller,omitempty"`
	Feature   string  `json:"feature,omitempty"`
	Agent     string  `json:"agent,omitempty"`
	GoalID    int64   `json:"goalId,omitempty"`
	Model     string  `json:"model,omitempty"`
	TokensIn  int64   `json:"tokensIn"`
	TokensOut int64   `json:"tokensOut"`
	CostUSD   float64 `json:"costUsd,omitempty"`
}

// UsageTotal is a sum over some entries.
type UsageTotal struct {
	Key       string  `json:"key"`
	Calls     int     `json:"calls"`
	TokensIn  int64   `json:"tokensIn"`
	TokensOut int64   `json:"tokensOut"`
	CostUSD   float64 `json:"costUsd"`
}

// UsageReport is GET /v1/usage.
type UsageReport struct {
	Since  int64        `json:"since"`
	Total  UsageTotal   `json:"total"`
	ByDay  []UsageTotal `json:"byDay"`
	ByGoal []UsageTotal `json:"byGoal"`
	// ByCaller is the agents ("agent") and each part that asked for answers ("norn").
	ByCaller []UsageTotal `json:"byCaller"`
	Window   UsageWindow  `json:"window"`
}

// UsageWindow is the plan's window: tokens used in it against the budget.
type UsageWindow struct {
	Hours  float64 `json:"hours"`
	Tokens int64   `json:"tokens"`
	Budget int64   `json:"budget,omitempty"`
	// Share is Tokens/Budget; 0 when no budget is set.
	Share float64 `json:"share"`
}

// Usage keeps the ledger.
type Usage struct {
	Store *Store
	// Gjallar gets the alert; nil is off.
	Gjallar *alert.Client
	// Budget is the tokens the plan allows in one Window; 0 turns the alert off.
	Budget int64
	Window time.Duration
	Logf   func(string, ...any)
	raised bool
}

// Record writes one entry down and looks at the window.
func (u *Usage) Record(e UsageEntry) {
	if u == nil || u.Store == nil || e.TokensIn+e.TokensOut == 0 && e.CostUSD == 0 {
		return
	}
	if e.At == 0 {
		e.At = u.Store.ms()
	}
	var goal any
	if e.GoalID == 0 && e.Agent != "" {
		_ = u.Store.db.QueryRow(`SELECT goal_id FROM tasks WHERE agent = ? AND goal_id IS NOT NULL ORDER BY id DESC LIMIT 1`, e.Agent).Scan(&e.GoalID)
	}
	if e.GoalID != 0 {
		goal = e.GoalID
	}
	if _, err := u.Store.db.Exec(`INSERT INTO usage (at, source, caller, feature, agent, goal_id, model, tokens_in, tokens_out, cost_usd) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.At, e.Source, e.Caller, e.Feature, e.Agent, goal, e.Model, e.TokensIn, e.TokensOut, e.CostUSD); err != nil {
		u.logf("usage: %v", err)
		return
	}
	u.check()
}

func (u *Usage) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

func (u *Usage) window() time.Duration {
	if u.Window <= 0 {
		return 5 * time.Hour
	}
	return u.Window
}

// Report totals the ledger from since (ms) on.
func (u *Usage) Report(since int64) (UsageReport, error) {
	r := UsageReport{Since: since, ByDay: []UsageTotal{}, ByGoal: []UsageTotal{}, ByCaller: []UsageTotal{}}
	group := func(key string) ([]UsageTotal, error) {
		rows, err := u.Store.db.Query(`SELECT `+key+` AS k, COUNT(*), SUM(tokens_in), SUM(tokens_out), SUM(cost_usd) FROM usage WHERE at >= ? GROUP BY k ORDER BY k DESC`, since)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []UsageTotal{}
		for rows.Next() {
			var t UsageTotal
			if err := rows.Scan(&t.Key, &t.Calls, &t.TokensIn, &t.TokensOut, &t.CostUSD); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}
	var err error
	if r.ByDay, err = group(`strftime('%Y-%m-%d', at/1000, 'unixepoch', 'localtime')`); err != nil {
		return r, err
	}
	if r.ByGoal, err = group(`CASE WHEN goal_id IS NULL THEN '' ELSE CAST(goal_id AS TEXT) END`); err != nil {
		return r, err
	}
	if r.ByCaller, err = group(`CASE WHEN source = 'agent' THEN 'agent' WHEN feature != '' THEN caller || ':' || feature ELSE caller END`); err != nil {
		return r, err
	}
	for _, d := range r.ByDay {
		r.Total.Calls += d.Calls
		r.Total.TokensIn += d.TokensIn
		r.Total.TokensOut += d.TokensOut
		r.Total.CostUSD += d.CostUSD
	}
	r.Total.Key = "total"
	r.Window = u.windowNow()
	return r, nil
}

func (u *Usage) windowNow() UsageWindow {
	w := UsageWindow{Hours: u.window().Hours(), Budget: u.Budget}
	from := u.Store.now().Add(-u.window()).UnixMilli()
	_ = u.Store.db.QueryRow(`SELECT COALESCE(SUM(tokens_in + tokens_out), 0) FROM usage WHERE at >= ?`, from).Scan(&w.Tokens)
	if u.Budget > 0 {
		w.Share = float64(w.Tokens) / float64(u.Budget)
	}
	return w
}

// check raises or resolves the alert by the share of the window used.
func (u *Usage) check() {
	if u.Budget <= 0 || u.Gjallar == nil {
		return
	}
	w := u.windowNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w.Share >= UsageAlertShare {
		err := u.Gjallar.Raise(ctx, alert.Raise{Key: "claude-usage", Severity: alert.Warning,
			Title:  "Claude usage is at " + strconv.Itoa(int(w.Share*100)) + "% of the plan's window",
			Detail: strconv.FormatInt(w.Tokens, 10) + " of " + strconv.FormatInt(w.Budget, 10) + " tokens in the last " + u.window().String() + ". See ygg usage."})
		u.raised = err == nil
		return
	}
	if u.raised {
		if err := u.Gjallar.Resolve(ctx, "claude-usage"); err == nil {
			u.raised = false
		}
	}
}

// claudeTokens reads the model and tokens out of `claude -p --output-format json`.
func claudeTokens(out []byte) (model string, in, outTokens int64, cost float64) {
	var raw struct {
		Cost  float64 `json:"total_cost_usd"`
		Usage struct {
			In        int64 `json:"input_tokens"`
			Out       int64 `json:"output_tokens"`
			CacheRead int64 `json:"cache_read_input_tokens"`
			CacheMake int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Models map[string]json.RawMessage `json:"modelUsage"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &raw) != nil {
		return "", 0, 0, 0
	}
	for name := range raw.Models {
		model = name
		break
	}
	u := raw.Usage
	return model, u.In + u.CacheRead + u.CacheMake, u.Out, raw.Cost
}

// agentUsage reads a Claude result event's usage.
func agentUsage(event map[string]any) (in, out int64) {
	usage, _ := event["usage"].(map[string]any)
	n := func(k string) int64 { f, _ := usage[k].(float64); return int64(f) }
	return n("input_tokens") + n("cache_read_input_tokens") + n("cache_creation_input_tokens"), n("output_tokens")
}

func (s *Sindri) usage(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		part.Error(w, http.StatusServiceUnavailable, "the usage ledger is off")
		return
	}
	since := time.Now().AddDate(0, 0, -7).UnixMilli()
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t.UnixMilli()
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			since = t.UnixMilli()
		} else if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = ms
		} else {
			part.Error(w, http.StatusBadRequest, "`since` is a date (2026-11-01), an RFC 3339 time or milliseconds")
			return
		}
	}
	report, err := s.Usage.Report(since)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, report)
}
