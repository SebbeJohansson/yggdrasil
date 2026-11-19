package huginn

// savepoint.ready: a meeting ended, and Norn hands back what you were doing
// when it started (its save point). The built-in save-point rule tells the
// widget only, never the phone: the last prompt is yours, and ntfy may be a
// public server.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"yggdrasil/internal/sse"
)

// SavePoint is Norn's savepoint.ready, the little of it Huginn reads.
type SavePoint struct {
	ID      int64  `json:"id"`
	Meeting string `json:"meeting"`
	Title   string `json:"title"`
	Link    string `json:"link"`
	ReadyAt *int64 `json:"readyAt"`
	Session *struct {
		Title      string `json:"title"`
		Project    string `json:"project"`
		Branch     string `json:"branch"`
		LastPrompt string `json:"lastPrompt"`
	} `json:"session"`
}

// savePointRule is the built-in rule for savepoint.ready: widget only, not held.
var savePointRule = Rule{ID: "save-point", Name: "What you were on before a meeting", On: Trigger{Event: "savepoint.ready"},
	Action: Action{Kind: "notify", Text: "{said}"}, Deliver: []string{"widget"}}

func (h *Huginn) fromSavePoint(event sse.Event) (Fired, bool) {
	var sp SavePoint
	if json.Unmarshal([]byte(event.Data), &sp) != nil || sp.ID == 0 || sp.Title == "" {
		return Fired{}, false
	}
	at := h.now()
	if sp.ReadyAt != nil {
		at = time.UnixMilli(*sp.ReadyAt)
	}
	link := ""
	if h.DashboardURL != "" && h.DashboardURL != "off" && sp.Link != "" {
		link = strings.TrimRight(h.DashboardURL, "/") + sp.Link
	}
	return Fired{Kind: event.Kind, Key: fmt.Sprintf("savepoint:%d", sp.ID), At: at, Title: "You were on: " + sp.Title, Link: link,
		Fields: savePointFields(sp)}, true
}

// savePointFields are the {fields} a savepoint.ready rule fills in; said is
// the whole message, calm and in one piece.
func savePointFields(sp SavePoint) map[string]string {
	fields := map[string]string{"title": sp.Title, "meeting": strings.TrimSpace(sp.Meeting), "prompt": "", "project": "", "branch": ""}
	before := "Before the meeting"
	if fields["meeting"] != "" {
		before = "Before " + fields["meeting"]
	}
	said := before + " you were on " + sp.Title + ". Pick it up when it suits."
	if s := sp.Session; s != nil {
		fields["prompt"], fields["project"], fields["branch"] = s.LastPrompt, s.Project, s.Branch
		if s.LastPrompt != "" {
			said = before + ", your last prompt was: " + s.LastPrompt
		}
	}
	fields["said"] = said
	return fields
}
