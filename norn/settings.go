package norn

import (
	"net/http"
	"slices"

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
	part.JSON(w, http.StatusOK, settings)
}
