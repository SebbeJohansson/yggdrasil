package huginn

// releaseRule is the built-in rule for a failed release or deploy: Munin's
// release:<repo> warning, told with the run's link so the run is one tap away.
// Production going down is already the critical alert rule's.
func releaseRule(deliver []string) Rule {
	return Rule{ID: "release-failed", Name: "A release or deploy failed",
		On:     Trigger{Event: "alert.opened", Source: "munin", KeyPrefix: "release:", Severity: []string{"warning", "critical"}},
		Action: Action{Kind: "notify", Text: "{title}. Open the run, or start an agent on its log."}, Deliver: deliver}
}
