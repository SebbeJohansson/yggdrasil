package norn

// The promise ledger's on/off switch (GET and PUT /v1/settings/promises). Off,
// Norn reads nothing you send or save for promises, writes no follow-ups and
// sends no reminders; the promises already kept stay listed, and Done, Drop
// and Change day still work. On until you turn it off.

import (
	"context"
	"net/http"

	"yggdrasil/internal/part"
)

const promisesOffKey = "promises-off"

// PromiseSettings is whether the ledger runs.
type PromiseSettings struct {
	On bool `json:"on"`
}

// On is whether the ledger runs; on when nothing is saved or it cannot be read.
func (p *Promises) On(ctx context.Context) bool {
	if p == nil {
		return false
	}
	var off bool
	if ok, err := p.n.Store.Setting(ctx, promisesOffKey, &off); err != nil || !ok {
		return true
	}
	return !off
}

func (n *Norn) promiseSettings(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, PromiseSettings{On: n.Promises.On(r.Context())})
}

func (n *Norn) setPromiseSettings(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner turns the promise ledger on or off")
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	if err := part.Decode(r, &body); err != nil || body.On == nil {
		part.Error(w, http.StatusBadRequest, "send `on`, true or false")
		return
	}
	if err := n.Store.SetSetting(r.Context(), promisesOffKey, !*body.On); err != nil {
		n.fail(w, err)
		return
	}
	if !*body.On {
		n.Promises.mu.Lock()
		clear(n.Promises.offered) // what was waiting to be read is not read later
		n.Promises.mu.Unlock()
	}
	n.logf("promise ledger: %s", map[bool]string{true: "on", false: "off"}[*body.On])
	part.JSON(w, http.StatusOK, PromiseSettings{On: *body.On})
}
