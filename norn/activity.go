package norn

// The activity heatmap: per day, your messages and agents' messages (Norn's
// own), joined with sessions, agents and merges (Sindri's).

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"yggdrasil/internal/part"
)

// DayActivity is one day of the heatmap.
type DayActivity struct {
	Messages      int `json:"messages"`
	AgentMessages int `json:"agentMessages"`
	Sessions      int `json:"sessions"`
	Agents        int `json:"agents"`
	Merges        int `json:"merges"`
}

// JoinActivity counts turns per day in zone, and adds Sindri's counts.
func JoinActivity(turns []TurnCount, fromSindri map[string]DayActivity, zone *time.Location) map[string]*DayActivity {
	days := map[string]*DayActivity{}
	day := func(key string) *DayActivity {
		if days[key] == nil {
			days[key] = &DayActivity{}
		}
		return days[key]
	}
	for _, t := range turns {
		d := day(time.UnixMilli(t.At).In(zone).Format("2006-01-02"))
		switch {
		case t.Author != nil:
			d.AgentMessages++
		case t.Role == "user":
			d.Messages++
		}
	}
	for key, counts := range fromSindri {
		d := day(key)
		d.Sessions += counts.Sessions
		d.Agents += counts.Agents
		d.Merges += counts.Merges
	}
	return days
}

// Activity is Sindri's sessions, agents and merges per day.
func (c *SindriClient) Activity(ctx context.Context, zone string, days int) (map[string]DayActivity, error) {
	var out map[string]DayActivity
	return out, c.call(ctx, http.MethodGet, "/v1/activity?tz="+url.QueryEscape(zone)+"&days="+strconv.Itoa(days), nil, &out)
}

func (n *Norn) activity(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 365
	}
	days = min(days, 800)
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	turns, err := n.Store.TurnsSince(r.Context(), since)
	if err != nil {
		n.fail(w, err)
		return
	}
	fromSindri := map[string]DayActivity{}
	if n.Sindri.On() {
		if fromSindri, err = n.Sindri.Activity(r.Context(), n.Config.TimeZone, days); err != nil {
			n.logf("no activity from Sindri: %v", err)
			fromSindri = map[string]DayActivity{}
		}
	}
	part.JSON(w, http.StatusOK, map[string]any{"timeZone": n.Config.TimeZone, "days": JoinActivity(turns, fromSindri, n.Zone)})
}
