package norn

// The person recap's on/off switch (GET and PUT /v1/settings/person-recap).
// Off, both recap routes answer empty with on false and read nothing. On
// until you turn it off.

import (
	"context"
	"net/http"

	"yggdrasil/internal/part"
)

const recapOffKey = "person-recap-off"

// On is whether recaps are given; on when nothing is saved or it cannot be read.
func (r *Recaps) On(ctx context.Context) bool {
	var off bool
	if ok, err := r.n.Store.Setting(ctx, recapOffKey, &off); err != nil || !ok {
		return true
	}
	return !off
}

func (n *Norn) mountRecapSettings(p *part.Part) {
	p.Handle("GET /v1/settings/person-recap", n.recapSettings)
	p.Handle("PUT /v1/settings/person-recap", n.setRecapSettings)
}

// RecapSettings is whether the person recap is given.
type RecapSettings struct {
	On bool `json:"on"`
}

func (n *Norn) recapSettings(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, RecapSettings{On: n.Recaps.On(r.Context())})
}

func (n *Norn) setRecapSettings(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner turns the person recap on or off")
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	if err := part.Decode(r, &body); err != nil || body.On == nil {
		part.Error(w, http.StatusBadRequest, "send `on`, true or false")
		return
	}
	if err := n.Store.SetSetting(r.Context(), recapOffKey, !*body.On); err != nil {
		n.fail(w, err)
		return
	}
	if !*body.On {
		n.Recaps.mu.Lock()
		clear(n.Recaps.kept)
		clear(n.Recaps.lines)
		n.Recaps.mu.Unlock()
	}
	n.logf("person recap: %s", map[bool]string{true: "on", false: "off"}[*body.On])
	part.JSON(w, http.StatusOK, RecapSettings{On: *body.On})
}
