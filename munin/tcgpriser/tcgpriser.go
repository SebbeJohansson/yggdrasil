// Package tcgpriser is Munin's source for tcgpriser.se: its API's health,
// the site's own status summary, the alerts it posts to Discord, and its API
// usage.
//
// Settings (in munin.env, with tcgpriser in MUNIN_SOURCES):
//
//	MUNIN_TCGPRISER_URL    the API (https://api.tcgpriser.se)
//	MUNIN_TCGPRISER_TOKEN  a service token from the API's API_AUTH_TOKENS. Without
//	                       one only the public /health is checked.
//
// Checks, raised on Gjallar as tcgpriser:<key>:
//
//	down    /health does not say healthy (critical)
//	status  /admin/status says degraded (warning) or down (critical)
//	admin   the admin routes do not answer (warning): a wrong token, or an API
//	        without them
//
// Every alert from /admin/alerts is raised as feed:tcgpriser:<its key> with
// its own severity, resolved when the site says so, and closed after 24 hours
// otherwise: the site counts an alert open for 24 hours too. Alerts from the
// shop scraping (source shop-import) are left out: they are the site's
// business, not Yggdrasil's. /admin/usage (the last 7 days) is read every 15
// minutes.
package tcgpriser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
	"yggdrasil/munin/source"
)

// Name is the source's name in MUNIN_SOURCES.
const Name = "tcgpriser"

// DefaultURL is the live API.
const DefaultURL = "https://api.tcgpriser.se"

const (
	every      = 2 * time.Minute
	usageEvery = 15 * time.Minute
	// eventLife is how long an alert the site never resolves stays open.
	eventLife = 24 * time.Hour
	pageSize  = 500
	// maxPages bounds one poll's catching up; the rest waits for the next.
	maxPages  = 10
	recentMax = 20
)

// ignored are the site's alert sources Munin leaves out: the shop scraping's
// import summaries and price sanity warnings.
var ignored = map[string]bool{"shop-import": true}

// Check keys.
const (
	DownKey   = "down"
	StatusKey = "status"
	AdminKey  = "admin"
)

// Source polls one tcgpriser API.
type Source struct {
	URL   string
	Token string
	HTTP  *http.Client

	now     func() time.Time
	usage   *Usage
	usageAt time.Time
	recent  []Alert
	primed  bool
}

// New is the source.Factory.
func New(settings source.Settings) (source.Source, error) {
	base := strings.TrimRight(settings("URL"), "/")
	if base == "" {
		base = DefaultURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("MUNIN_TCGPRISER_URL: %q is not an http(s) URL", base)
	}
	return &Source{URL: base, Token: settings("TOKEN"), HTTP: &http.Client{Timeout: 15 * time.Second}, now: time.Now, recent: []Alert{}}, nil
}

func (s *Source) Name() string         { return Name }
func (s *Source) Every() time.Duration { return every }

// Health is GET /health's answer.
type Health struct {
	Status  string  `json:"status"`
	MongoDB string  `json:"mongodb"`
	Uptime  float64 `json:"uptime"`
}

// Status is GET /admin/status's answer.
type Status struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
	MongoDB   string    `json:"mongodb"`
	Delivery  struct {
		Configured   bool       `json:"configured"`
		Ready        *bool      `json:"ready"`
		FailingSince *time.Time `json:"failingSince"`
	} `json:"delivery"`
	Cmapi struct {
		Cap             int        `json:"cap"`
		Used            int        `json:"used"`
		Remaining       int        `json:"remaining"`
		PlanDailyBudget int        `json:"planDailyBudget"`
		WindowEndsAt    *time.Time `json:"windowEndsAt"`
		PausedUntil     *time.Time `json:"pausedUntil"`
	} `json:"cmapi"`
	Freshness []struct {
		Source              string     `json:"source"`
		Trigger             string     `json:"trigger"`
		LastSentAt          *time.Time `json:"lastSentAt"`
		ConsecutiveZeroRuns int        `json:"consecutiveZeroRuns"`
	} `json:"freshness"`
	OpenAlerts int `json:"openAlerts"`
}

// Alert is one of GET /admin/alerts' items, less its data.
type Alert struct {
	ID        string         `json:"id"`
	Source    string         `json:"source"`
	Trigger   string         `json:"trigger"`
	Severity  alert.Severity `json:"severity"`
	Key       string         `json:"key"`
	Resolved  bool           `json:"resolved"`
	Title     string         `json:"title"`
	Message   string         `json:"message,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

type alertPage struct {
	Items      []Alert `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// Counts is a share of the API's traffic.
type Counts struct {
	Requests     int     `json:"requests"`
	ClientErrors int     `json:"clientErrors"`
	ServerErrors int     `json:"serverErrors"`
	AvgMs        float64 `json:"avgMs"`
}

// Usage is GET /admin/usage's answer for the API's default range (the last
// 7 days), without the callers' email addresses.
type Usage struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Totals       Counts    `json:"totals"`
	ByCallerType []struct {
		CallerType string `json:"callerType"`
		Counts
		Callers int `json:"callers"`
	} `json:"byCallerType"`
	TopCallers  []Caller `json:"topCallers"`
	ByOperation []struct {
		OperationID string `json:"operationId"`
		Counts
	} `json:"byOperation"`
}

// Caller is one of the busiest callers.
type Caller struct {
	CallerType string `json:"callerType"`
	CallerID   string `json:"callerId"`
	Counts
	// Name is the account's name, for a signed-in caller.
	Name string `json:"name,omitempty"`
}

// Data is what the dashboard shows of the site.
type Data struct {
	API string `json:"api"`
	// Admin is whether a token is set, so more than /health is read.
	Admin  bool    `json:"admin"`
	Health *Health `json:"health,omitempty"`
	Status *Status `json:"status,omitempty"`
	Usage  *Usage  `json:"usage,omitempty"`
	// Recent is the site's latest alerts, newest first.
	Recent []Alert `json:"recent"`
}

// Poll checks the site once.
func (s *Source) Poll(ctx context.Context, cursor string) source.Report {
	data := Data{API: s.URL, Admin: s.Token != "", Recent: s.recent}
	report := source.Report{Data: &data}
	down := watch.Result{Key: DownKey, OK: true, Severity: alert.Critical, Title: "tcgpriser's API is down"}
	var health Health
	if err := s.get(ctx, "/health", false, &health); err != nil || health.Status != "healthy" {
		down.OK = false
		down.Detail = errorOr(err, "status "+health.Status+", mongodb "+health.MongoDB)
		report.Detail = down.Detail
		// One problem, one alert: the rest waits until it answers.
		report.Checks = []watch.Result{down, {Key: StatusKey, Skip: true}, {Key: AdminKey, Skip: true}}
		if err == nil {
			data.Health = &health
		}
		return report
	}
	data.Health = &health
	report.Up = true
	report.Detail = "healthy"

	if s.Token == "" {
		report.Detail = "healthy; no token, so health only"
		report.Checks = []watch.Result{down, {Key: StatusKey, OK: true}, {Key: AdminKey, OK: true}}
		return report
	}

	admin := watch.Result{Key: AdminKey, OK: true, Severity: alert.Warning, Title: "Munin can't read tcgpriser's admin API"}
	fail := func(what string, err error) {
		if admin.OK {
			admin.OK, admin.Detail = false, what+": "+err.Error()
		}
	}

	statusCheck := watch.Result{Key: StatusKey, Skip: true}
	var status Status
	if err := s.get(ctx, "/admin/status", true, &status); err != nil {
		fail("/admin/status", err)
	} else {
		data.Status = &status
		statusCheck = StatusCheck(status, s.now())
		if !statusCheck.OK {
			report.Detail = status.Status
		}
	}

	events, next, err := s.feed(ctx, cursor)
	if err != nil {
		fail("/admin/alerts", err)
	}
	report.Events, report.Cursor = events, next
	if !s.primed && cursor != "" && err == nil {
		// After a restart the feed starts where it left off; the dashboard
		// still wants the last day's alerts.
		if page, err := s.page(ctx, ""); err == nil {
			s.remember(page.Items)
		}
	}
	s.primed = s.primed || err == nil
	data.Recent = s.recent

	if s.usage == nil || s.now().Sub(s.usageAt) >= usageEvery {
		if usage, err := s.fetchUsage(ctx); err != nil {
			fail("/admin/usage", err)
		} else {
			s.usage, s.usageAt = usage, s.now()
		}
	}
	data.Usage = s.usage

	if !admin.OK && report.Detail == "healthy" {
		report.Detail = "healthy; admin API unreadable"
	}
	report.Checks = []watch.Result{down, statusCheck, admin}
	return report
}

// StatusCheck is the status check for /admin/status's answer.
func StatusCheck(status Status, now time.Time) watch.Result {
	check := watch.Result{Key: StatusKey, OK: status.Status == "ok", Severity: alert.Warning}
	if check.OK {
		return check
	}
	if status.Status == "down" {
		check.Severity = alert.Critical
	}
	check.Title = "tcgpriser is " + status.Status
	var reasons []string
	if status.MongoDB != "connected" {
		reasons = append(reasons, "MongoDB "+status.MongoDB)
	}
	if status.Delivery.Configured && status.Delivery.Ready != nil && !*status.Delivery.Ready {
		reason := "delivery not ready"
		if since := status.Delivery.FailingSince; since != nil {
			reason += fmt.Sprintf(" for %d min", int(now.Sub(*since).Minutes()))
		}
		reasons = append(reasons, reason)
	}
	if until := status.Cmapi.PausedUntil; until != nil && until.After(now) {
		reasons = append(reasons, "cmapi paused until "+until.Local().Format("15:04"))
	}
	if status.OpenAlerts > 0 {
		reasons = append(reasons, fmt.Sprintf("%d open alert(s)", status.OpenAlerts))
	}
	check.Detail = strings.Join(reasons, "; ")
	return check
}

// feed reads the alerts after cursor, oldest first.
func (s *Source) feed(ctx context.Context, cursor string) ([]source.Event, string, error) {
	var events []source.Event
	for range maxPages {
		page, err := s.page(ctx, cursor)
		if err != nil {
			// Hand back what was read and how far: Munin keeps that cursor
			// once those events reached Gjallar.
			return events, cursor, err
		}
		for _, item := range page.Items {
			if !ignored[item.Source] {
				events = append(events, Event(item))
			}
		}
		s.remember(page.Items)
		if page.NextCursor != nil {
			cursor = *page.NextCursor
		}
		if len(page.Items) < pageSize {
			break
		}
	}
	return events, cursor, nil
}

// Event is how one site alert is raised on Gjallar.
func Event(item Alert) source.Event {
	severity := item.Severity
	if severity != alert.Info && severity != alert.Warning && severity != alert.Critical {
		severity = alert.Warning
	}
	expires := item.CreatedAt.Add(eventLife)
	return source.Event{
		Key:       item.Key,
		Resolved:  item.Resolved,
		Severity:  severity,
		Title:     "tcgpriser: " + item.Title,
		Detail:    item.Message,
		ExpiresAt: &expires,
	}
}

func (s *Source) page(ctx context.Context, cursor string) (alertPage, error) {
	query := url.Values{"limit": {fmt.Sprint(pageSize)}}
	if cursor != "" {
		query.Set("after", cursor)
	}
	var page alertPage
	err := s.get(ctx, "/admin/alerts?"+query.Encode(), true, &page)
	return page, err
}

// remember keeps the newest alerts for the dashboard, newest first.
func (s *Source) remember(all []Alert) {
	items := make([]Alert, 0, len(all))
	for _, item := range all {
		if !ignored[item.Source] {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return
	}
	recent := make([]Alert, 0, recentMax)
	for i := len(items) - 1; i >= 0 && len(recent) < recentMax; i-- {
		recent = append(recent, items[i])
	}
	seen := map[string]bool{}
	for _, item := range recent {
		seen[item.ID] = true
	}
	for _, item := range s.recent {
		if len(recent) >= recentMax {
			break
		}
		if !seen[item.ID] {
			recent = append(recent, item)
		}
	}
	s.recent = recent
}

func (s *Source) fetchUsage(ctx context.Context) (*Usage, error) {
	// No from or to: the API's own range, the last 7 days. Its from takes
	// nothing today: the contract's check wants a date-time, the handler's a
	// YYYY-MM-DD, so every value is refused by one of them.
	query := url.Values{"limit": {"10"}}
	var raw struct {
		Usage
		TopCallers []struct {
			Caller
			User *struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"topCallers"`
	}
	if err := s.get(ctx, "/admin/usage?"+query.Encode(), true, &raw); err != nil {
		return nil, err
	}
	usage := raw.Usage
	usage.TopCallers = make([]Caller, 0, len(raw.TopCallers))
	for _, caller := range raw.TopCallers {
		if caller.User != nil {
			caller.Caller.Name = caller.User.Name
		}
		usage.TopCallers = append(usage.TopCallers, caller.Caller)
	}
	return &usage, nil
}

func (s *Source) get(ctx context.Context, path string, auth bool, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "yggdrasil-munin")
	request.Header.Set("Accept", "application/json")
	if auth {
		request.Header.Set("Authorization", "Bearer "+s.Token)
	}
	response, err := s.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	// /health answers 503 with its body when unhealthy: read it anyway.
	if response.StatusCode >= 300 && !(path == "/health" && response.StatusCode == http.StatusServiceUnavailable) {
		return fmt.Errorf("answers %d%s", response.StatusCode, apiError(body))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("unexpected answer: %w", err)
	}
	return nil
}

// apiError is the API's error message, when the body has one.
func apiError(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return ": " + parsed.Error.Message
	}
	return ""
}

func errorOr(err error, otherwise string) string {
	if err != nil {
		return err.Error()
	}
	return otherwise
}
