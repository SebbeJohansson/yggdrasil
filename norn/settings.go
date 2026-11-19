package norn

import (
	"context"
	"net/http"
	"slices"
	"time"

	"yggdrasil/internal/part"
)

// WidgetFeatures are what can be hidden at home or away: my-widget's windows
// (agents, inbox, ask), the inbox's sources, and the two calendars. A hidden
// source is not fetched at all.
var WidgetFeatures = []string{"agents", "inbox", "ask", "teams", "slack", "outlook", "outlook-calendar", "gmail", "google-calendar"}

// WidgetSettings say where this computer is, and what to hide in each place.
type WidgetSettings struct {
	Place      string   `json:"place"`
	HideAtHome []string `json:"hideAtHome"`
	HideAway   []string `json:"hideAway"`
}

const widgetSettingsKey = "widget-visibility"

// Hidden is whether a feature is switched off where this computer is now.
// Nothing is, while the place is unknown.
func (s WidgetSettings) Hidden(feature string) bool {
	switch s.Place {
	case "home":
		return slices.Contains(s.HideAtHome, feature)
	case "away":
		return slices.Contains(s.HideAway, feature)
	}
	return false
}

func (n *Norn) loadWidgetSettings(r *http.Request) (WidgetSettings, error) {
	settings := WidgetSettings{Place: "unknown", HideAtHome: []string{}, HideAway: []string{}}
	_, err := n.Store.Setting(r.Context(), widgetSettingsKey, &settings)
	return settings, err
}

func (n *Norn) widgetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := n.loadWidgetSettings(r)
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, settings)
}

func uniqueFeatures(list []string) ([]string, bool) {
	kept := []string{}
	for _, item := range list {
		if !slices.Contains(WidgetFeatures, item) {
			return nil, false
		}
		if !slices.Contains(kept, item) {
			kept = append(kept, item)
		}
	}
	return kept, true
}

func (n *Norn) setWidgetSettings(w http.ResponseWriter, r *http.Request) {
	var body WidgetSettings
	err := part.Decode(r, &body)
	atHome, homeOK := uniqueFeatures(body.HideAtHome)
	away, awayOK := uniqueFeatures(body.HideAway)
	if err != nil || !slices.Contains([]string{"home", "away", "unknown"}, body.Place) || body.HideAtHome == nil || body.HideAway == nil || !homeOK || !awayOK {
		part.Error(w, http.StatusBadRequest, "`place` must be home, away or unknown, and hideAtHome/hideAway must be lists of: agents, inbox, ask, teams, slack, outlook, outlook-calendar, gmail, google-calendar")
		return
	}
	settings := WidgetSettings{Place: body.Place, HideAtHome: atHome, HideAway: away}
	if err := n.Store.SetSetting(r.Context(), widgetSettingsKey, settings); err != nil {
		n.fail(w, err)
		return
	}
	// A source switched on or off shows now, not at the next poll.
	go func() {
		ctx := context.Background()
		_ = n.Inbox.SweepSlack(ctx)
		_ = n.readWorkCalendar(ctx)
		_ = n.readPersonalCalendar(ctx)
	}()
	part.JSON(w, http.StatusOK, settings)
}

// namesOffKey is when names may go to Claude until (Unix ms): a way round a
// local model that cannot look for them, which turns itself back off.
const namesOffKey = "send-names-until"

// NamesOff is whether names go to Claude as they are, without placeholders.
func (n *Norn) NamesOff(ctx context.Context) bool {
	var until int64
	if ok, err := n.Store.Setting(ctx, namesOffKey, &until); err != nil || !ok {
		return false
	}
	return time.Now().UnixMilli() < until
}

// NamesOffSettings are until when names go to Claude unswapped (0: they don't).
type NamesOffSettings struct {
	Until int64 `json:"until"`
}

func (n *Norn) namesOffSettings(w http.ResponseWriter, r *http.Request) {
	var until int64
	if _, err := n.Store.Setting(r.Context(), namesOffKey, &until); err != nil {
		n.fail(w, err)
		return
	}
	if until < time.Now().UnixMilli() {
		until = 0
	}
	part.JSON(w, http.StatusOK, NamesOffSettings{Until: until})
}

// setNamesOff sends names to Claude unswapped for {"hours": N}, at most 24;
// 0 swaps them again.
func (n *Norn) setNamesOff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hours *float64 `json:"hours"`
	}
	if err := part.Decode(r, &body); err != nil || body.Hours == nil || *body.Hours < 0 || *body.Hours > 24 {
		part.Error(w, http.StatusBadRequest, "`hours` must be 0 to 24 (0: names are swapped again)")
		return
	}
	var until int64
	if *body.Hours > 0 {
		until = time.Now().Add(time.Duration(*body.Hours * float64(time.Hour))).UnixMilli()
	}
	if err := n.Store.SetSetting(r.Context(), namesOffKey, until); err != nil {
		n.fail(w, err)
		return
	}
	n.logf("names to Claude: %s", map[bool]string{true: "sent as they are until " + time.UnixMilli(until).Format("15:04"), false: "swapped for placeholders"}[until > 0])
	part.JSON(w, http.StatusOK, NamesOffSettings{Until: until})
}
