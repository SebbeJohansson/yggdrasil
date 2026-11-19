package heimdall

// scopes: the access Norn's sign-ins hold that nothing uses. Norn says which
// scopes each sign-in holds (GET /v1/microsoft, GET /v1/google) and what
// the widget hides at home and away (GET /v1/settings/widgets). A scope
// whose feature is hidden in both places is held for nothing: sign in again
// without it.

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"yggdrasil/internal/call"
)

// Scopes is the scopes check.
type Scopes struct {
	// Norn is Heimdall's token at Norn; off without NORN_TOKEN.
	Norn *call.Part
}

// Name is "scopes".
func (*Scopes) Name() string { return "scopes" }

// grant is one scope a sign-in may hold, and the widget features it feeds.
type grant struct {
	provider, scope, label string
	held                   bool
	features               []string
}

// Run asks Norn.
func (c *Scopes) Run(ctx context.Context, _ *Env) (Result, error) {
	if c.Norn == nil || !c.Norn.On() {
		return Result{Skipped: "no NORN_TOKEN: Heimdall can't ask Norn which sign-ins it holds"}, nil
	}
	var microsoft struct {
		State  string `json:"state"`
		Scopes *struct {
			Mail     bool `json:"mail"`
			Calendar bool `json:"calendar"`
		} `json:"scopes"`
	}
	var google struct {
		State  string `json:"state"`
		Grants *struct {
			Gmail    bool `json:"gmail"`
			Calendar bool `json:"calendar"`
			Tasks    bool `json:"tasks"`
		} `json:"grants"`
	}
	var widgets struct {
		HideAtHome []string `json:"hideAtHome"`
		HideAway   []string `json:"hideAway"`
	}
	if err := c.Norn.Do(ctx, http.MethodGet, "/v1/settings/widgets", nil, &widgets); err != nil {
		return Result{}, fmt.Errorf("Norn's widget settings: %w", err)
	}
	var grants []grant
	if err := c.Norn.Do(ctx, http.MethodGet, "/v1/microsoft", nil, &microsoft); err != nil {
		return Result{}, fmt.Errorf("Norn's Microsoft sign-in: %w", err)
	}
	if microsoft.State == "signed-in" && microsoft.Scopes != nil {
		grants = append(grants,
			grant{"microsoft", "mail", "Outlook mail", microsoft.Scopes.Mail, []string{"outlook"}},
			grant{"microsoft", "calendar", "the Outlook calendar", microsoft.Scopes.Calendar, []string{"outlook-calendar"}})
	}
	if err := c.Norn.Do(ctx, http.MethodGet, "/v1/google", nil, &google); err != nil {
		return Result{}, fmt.Errorf("Norn's Google sign-in: %w", err)
	}
	if google.State == "signed-in" && google.Grants != nil {
		grants = append(grants,
			grant{"google", "gmail", "Gmail", google.Grants.Gmail, []string{"gmail"}},
			grant{"google", "calendar", "the Google calendar", google.Grants.Calendar, []string{"google-calendar"}},
			grant{"google", "tasks", "Google Tasks", google.Grants.Tasks, []string{"google-calendar"}})
	}
	findings := []Finding{}
	for _, g := range grants {
		if !g.held || slices.ContainsFunc(g.features, func(f string) bool {
			return !slices.Contains(widgets.HideAtHome, f) || !slices.Contains(widgets.HideAway, f)
		}) {
			continue
		}
		provider := map[string]string{"microsoft": "Microsoft", "google": "Google"}[g.provider]
		findings = append(findings, Finding{
			ID:       StableID(g.provider, g.scope),
			Severity: Low,
			Title:    fmt.Sprintf("Norn holds access to %s, which nothing uses", g.label),
			Detail:   fmt.Sprintf("The %s sign-in includes %s, but the widget hides it both at home and away, so Norn never reads it.", provider, g.label),
			Where:    "Norn's " + provider + " sign-in",
			Fix:      fmt.Sprintf("Sign in to %s again from Settings without %s, or show it again if you want it.", provider, g.label),
		})
	}
	return Result{Findings: findings}, nil
}
