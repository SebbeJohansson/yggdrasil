package huginn

// Promises you made (Norn's promise ledger). Norn publishes promise.due once
// per promise, on the working day before its day or on the day, and the
// built-in promise-due rule tells you through Hermóðr, held through meetings.
// Every 5 minutes Huginn also asks Norn (with /local, so the promise stays on
// this machine) whether each open promise not yet judged is development
// work, the same question as for a Linear ticket, and keeps the answer on
// the promise (PUT /v1/promises/{id}/dev): the dashboard then offers to make
// it a goal. Huginn makes no goal itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"yggdrasil/internal/sse"
)

// PromiseAsk is what Norn is asked about a promise.
const PromiseAsk = `Is this promise software development work: writing, changing or fixing code? Research, admin, meetings, sending or checking something, and decisions are not. Answer with one word, yes or no.

Promise: %s`

// PromiseDue is Norn's promise.due.
type PromiseDue struct {
	ID     int64  `json:"id"`
	What   string `json:"what"`
	To     string `json:"to"`
	Due    string `json:"due"`
	By     string `json:"by"`
	Source string `json:"source"`
	Draft  bool   `json:"draft"`
	Dev    bool   `json:"dev"`
	Link   string `json:"link"`
	At     int64  `json:"at"`
}

// promiseRule is the built-in rule for promise.due.
func promiseRule(both []string) Rule {
	return Rule{ID: "promise-due", Name: "A promise you made is due", On: Trigger{Event: "promise.due"},
		Action: Action{Kind: "notify", Text: "{promised}: {what}, by {by}.{also}"}, Deliver: both, Hold: true}
}

// fromPromise reads promise.due.
func (h *Huginn) fromPromise(event sse.Event) (Fired, bool) {
	var due PromiseDue
	if json.Unmarshal([]byte(event.Data), &due) != nil || due.ID == 0 {
		return Fired{}, false
	}
	promised := "You promised"
	if to := strings.TrimSpace(due.To); to != "" {
		promised += " " + to
	}
	also := ""
	switch {
	case due.Draft:
		also = " A reply is drafted on the dashboard."
	case due.Dev:
		also = " You can make it a goal from the dashboard."
	}
	at := h.now()
	if due.At > 0 {
		at = time.UnixMilli(due.At)
	}
	return Fired{Kind: event.Kind, Key: fmt.Sprintf("promise.due:%d:%s", due.ID, due.Due), At: at, Title: "A promise for " + due.By, Link: due.Link,
		Fields: map[string]string{"promised": promised, "to": due.To, "what": due.What, "by": due.By, "due": due.Due, "source": due.Source, "also": also, "link": due.Link}}, true
}

// openPromise is the little of Norn's promise Huginn reads.
type openPromise struct {
	ID   int64  `json:"id"`
	What string `json:"what"`
	Dev  *bool  `json:"dev"`
}

// watchPromises judges each open promise once: development work or not.
func (h *Huginn) watchPromises(ctx context.Context) error {
	if !h.Norn.On() {
		return nil
	}
	var answer struct {
		Promises []openPromise `json:"promises"`
	}
	if err := h.Norn.Do(ctx, http.MethodGet, "/v1/promises?state=open", nil, &answer); err != nil {
		return err
	}
	var failed []error
	asked := 0
	for _, p := range answer.Promises {
		if p.Dev != nil || strings.TrimSpace(p.What) == "" {
			continue
		}
		asked++
		reply, err := h.Norn.Ask(ctx, fmt.Sprintf(PromiseAsk, clipText(p.What, 300)), AskSession)
		if err != nil {
			failed = append(failed, fmt.Errorf("asking Norn about promise %d: %w", p.ID, err))
			continue
		}
		dev := IsYes(reply)
		if err := h.Norn.Do(ctx, http.MethodPut, fmt.Sprintf("/v1/promises/%d/dev", p.ID), map[string]bool{"dev": dev}, nil); err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 && len(failed) == asked {
		return errors.Join(failed...)
	}
	for _, err := range failed {
		h.logf("promises: %v", err)
	}
	return nil
}
