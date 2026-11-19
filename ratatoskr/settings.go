package ratatoskr

// Watching you on or off from the dashboard: GET and PATCH /v1/settings/you.
// What you set is kept in a small file (RATATOSKR_STATE), so it outlives a
// restart; RATATOSKR_YOU is the default until you set it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"yggdrasil/internal/part"
)

// State is what Ratatoskr keeps across restarts.
type State struct {
	// You is watching you on or off, as you last set it; nil until you do.
	You *YouSetting `json:"you,omitempty"`
}

// YouSetting is GET and PATCH /v1/settings/you.
type YouSetting struct {
	On bool `json:"on"`
}

// LoadState reads the state file; a missing one is an empty state.
func LoadState(path string) (State, error) {
	var state State
	if path == "" {
		return state, nil
	}
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(text, &state); err != nil {
		return state, fmt.Errorf("%s: %w", path, err)
	}
	return state, nil
}

// SaveState writes the state file, aside and renamed, so a crash never
// leaves half a file.
func SaveState(path string, state State) error {
	if path == "" {
		return nil
	}
	text, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, text, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// IsOn is whether it is watching you now.
func (y *You) IsOn() bool {
	if y == nil {
		return false
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.on
}

// SetOn turns watching you on or off and keeps it in the state file. Off
// clears what it saw; on looks again at once, in the background.
func (y *You) SetOn(on bool) error {
	y.mu.Lock()
	if y.StatePath != "" {
		state, err := LoadState(y.StatePath)
		if err == nil {
			state.You = &YouSetting{On: on}
			err = SaveState(y.StatePath, state)
		}
		if err != nil {
			y.mu.Unlock()
			return err
		}
	}
	was := y.on
	y.on = on
	if !on {
		y.status = YouStatus{On: false, Patterns: []Pattern{}}
	} else if !was {
		y.status = YouStatus{On: true, Patterns: []Pattern{}}
	}
	y.mu.Unlock()
	if on && !was {
		go func() { _ = y.Round(context.Background()) }()
	}
	return nil
}

func (r *Ratatoskr) getYouSetting(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, YouSetting{On: r.You.IsOn()})
}

func (r *Ratatoskr) patchYouSetting(w http.ResponseWriter, req *http.Request) {
	if part.Caller(req) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may turn watching you on or off")
		return
	}
	var body struct {
		On *bool `json:"on"`
	}
	if err := part.Decode(req, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.On == nil {
		part.Error(w, http.StatusBadRequest, "on is true or false")
		return
	}
	if r.You == nil {
		part.Error(w, http.StatusConflict, "watching you is not set up here")
		return
	}
	if err := r.You.SetOn(*body.On); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, YouSetting{On: r.You.IsOn()})
}
