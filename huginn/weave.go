package huginn

// weave.ready: Norn made the week's plan (the weave). The built-in week-plan
// rule tells the widget only, once, calm and not held: the plan waits on the
// dashboard until you book or skip it, and nothing is pushed to the phone.

import (
	"encoding/json"
	"strings"
	"time"

	"yggdrasil/internal/sse"
)

// WeaveReady is Norn's weave.ready.
type WeaveReady struct {
	Week   string `json:"week"`
	Words  string `json:"words"`
	MadeAt int64  `json:"madeAt"`
	Link   string `json:"link"`
}

// weaveRule is the built-in rule for weave.ready: the widget only, not held.
var weaveRule = Rule{ID: "week-plan", Name: "The week's plan is ready", On: Trigger{Event: "weave.ready"},
	Action: Action{Kind: "notify", Text: "{words}"}, Deliver: []string{"widget"}}

func (h *Huginn) fromWeave(event sse.Event) (Fired, bool) {
	var ready WeaveReady
	if json.Unmarshal([]byte(event.Data), &ready) != nil || ready.Week == "" {
		return Fired{}, false
	}
	at := h.now()
	if ready.MadeAt > 0 {
		at = time.UnixMilli(ready.MadeAt)
	}
	link := ""
	if h.DashboardURL != "" && h.DashboardURL != "off" && ready.Link != "" {
		link = strings.TrimRight(h.DashboardURL, "/") + ready.Link
	}
	words := strings.TrimSpace(ready.Words)
	if words == "" {
		words = "The plan for the week is ready when you are."
	}
	return Fired{Kind: event.Kind, Key: "weave:" + ready.Week, At: at, Title: "The week's plan", Link: link,
		Fields: map[string]string{"week": ready.Week, "words": words}}, true
}
