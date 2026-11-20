package norn

// Model health: every model call Norn made in the last day, counted by hour
// and by purpose, with failures sorted into kinds. A model that quietly fails
// (XeStrata unable to answer in JSON, timing out, not running) shows here
// before it shows as a part that stopped doing its job.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
)

const (
	healthMaxHours = 72
	healthRecent   = 10

	// The model-health loop raises "the local model is failing" when, over
	// the last healthWindow, at least healthMinFailed calls failed for good
	// and they were at least healthFailShare of the calls.
	healthEvery     = 5 * time.Minute
	healthWindow    = time.Hour
	healthMinFailed = 5
	healthFailShare = 0.5
	healthAlertKey  = "model-failing"

	// retryPurpose is an answer asked again without JSON mode (ask.go): when
	// it answers, the failed JSON attempt before it was recovered.
	retryPurpose = "answer without JSON"
)

// Failure kinds, from the error text.
const (
	failJSON        = "json"
	failTimeout     = "timeout"
	failUnreachable = "unreachable"
	failOther       = "other"
)

// FailureKind sorts a model call's error into json, timeout, unreachable or other.
func FailureKind(err string) string {
	lower := strings.ToLower(err)
	switch {
	case strings.Contains(lower, "structured_output") || strings.Contains(lower, "json"):
		return failJSON
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") || strings.Contains(lower, "aborted") || strings.Contains(lower, "canceled"):
		return failTimeout
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "fetch failed") || strings.Contains(lower, "no such host") || strings.Contains(lower, "no model server"):
		return failUnreachable
	default:
		return failOther
	}
}

// HealthCall is one model call, as the health view counts it.
type HealthCall struct {
	At      int64
	Purpose string
	Backend string
	TotalMS int64
	Error   string
}

// HealthCalls is every model call since `since`, oldest first, without its text.
func (s *Store) HealthCalls(ctx context.Context, since int64) ([]HealthCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, purpose, backend, total_ms, COALESCE(error, '') FROM llm_calls WHERE at >= ? AND COALESCE(error, '') NOT LIKE 'kept on this machine%' ORDER BY at, id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := []HealthCall{}
	for rows.Next() {
		var c HealthCall
		if err := rows.Scan(&c.At, &c.Purpose, &c.Backend, &c.TotalMS, &c.Error); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, rows.Err()
}

// HealthCount is calls, how many failed (by kind), and how many Ollama answered
// because XeStrata did not.
type HealthCount struct {
	Calls  int            `json:"calls"`
	Failed int            `json:"failed"`
	Ollama int            `json:"ollama"`
	Kinds  map[string]int `json:"kinds"`
}

func (h *HealthCount) add(c HealthCall) {
	h.Calls++
	if c.Backend == "ollama" {
		h.Ollama++
	}
	if c.Error != "" {
		h.Failed++
		if h.Kinds == nil {
			h.Kinds = map[string]int{}
		}
		h.Kinds[FailureKind(c.Error)]++
	}
}

// HealthHour is one hour's calls; `at` is the hour's start.
type HealthHour struct {
	At int64 `json:"at"`
	HealthCount
}

// HealthPurpose is one purpose's calls, with its median time.
type HealthPurpose struct {
	Purpose  string `json:"purpose"`
	MedianMS int64  `json:"medianMs"`
	HealthCount
}

// HealthFailure is one failed call, its error clipped.
type HealthFailure struct {
	At      int64  `json:"at"`
	Purpose string `json:"purpose"`
	Backend string `json:"backend"`
	Kind    string `json:"kind"`
	Error   string `json:"error"`
}

// ModelHealth is the health view: totals, each hour (oldest first, every hour
// present), each purpose (most failures first), and the latest failures.
type ModelHealth struct {
	Since    int64           `json:"since"`
	Total    HealthCount     `json:"total"`
	Hours    []HealthHour    `json:"hours"`
	Purposes []HealthPurpose `json:"purposes"`
	Recent   []HealthFailure `json:"recent"`
	// LastHour is the alert's view: failures no retry made good, of the calls.
	LastHour LastHour `json:"lastHour"`
}

// LastHour is the last hour as the alert judges it.
type LastHour struct {
	Calls   int  `json:"calls"`
	Failed  int  `json:"failed"`
	Failing bool `json:"failing"`
}

// SummarizeHealth counts calls over the `hours` hours up to `now`.
func SummarizeHealth(calls []HealthCall, now time.Time, hours int) ModelHealth {
	hour := int64(time.Hour / time.Millisecond)
	first := now.Truncate(time.Hour).UnixMilli() - int64(hours-1)*hour
	health := ModelHealth{Since: first, Hours: make([]HealthHour, hours), Purposes: []HealthPurpose{}, Recent: []HealthFailure{}}
	for i := range health.Hours {
		health.Hours[i].At = first + int64(i)*hour
	}
	byPurpose := map[string]*HealthPurpose{}
	times := map[string][]int64{}
	for _, c := range calls {
		if c.At < first {
			continue
		}
		health.Total.add(c)
		if i := int((c.At - first) / hour); i < hours {
			health.Hours[i].add(c)
		}
		p, ok := byPurpose[c.Purpose]
		if !ok {
			p = &HealthPurpose{Purpose: c.Purpose}
			byPurpose[c.Purpose] = p
		}
		p.add(c)
		if c.Error == "" {
			times[c.Purpose] = append(times[c.Purpose], c.TotalMS)
		} else {
			text := c.Error
			if r := []rune(text); len(r) > 200 {
				text = string(r[:200]) + "…"
			}
			health.Recent = append(health.Recent, HealthFailure{At: c.At, Purpose: c.Purpose, Backend: c.Backend, Kind: FailureKind(c.Error), Error: text})
		}
	}
	for purpose, p := range byPurpose {
		if t := times[purpose]; len(t) > 0 {
			sort.Slice(t, func(i, j int) bool { return t[i] < t[j] })
			p.MedianMS = t[len(t)/2]
		}
		health.Purposes = append(health.Purposes, *p)
	}
	sort.Slice(health.Purposes, func(i, j int) bool {
		a, b := health.Purposes[i], health.Purposes[j]
		if a.Failed != b.Failed {
			return a.Failed > b.Failed
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return a.Purpose < b.Purpose
	})
	// Newest failures first.
	for i, j := 0, len(health.Recent)-1; i < j; i, j = i+1, j-1 {
		health.Recent[i], health.Recent[j] = health.Recent[j], health.Recent[i]
	}
	if len(health.Recent) > healthRecent {
		health.Recent = health.Recent[:healthRecent]
	}
	return health
}

func (n *Norn) modelHealth(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > healthMaxHours {
			part.Error(w, http.StatusBadRequest, "`hours` must be 1-72")
			return
		}
		hours = parsed
	}
	now := time.Now()
	calls, err := n.Store.HealthCalls(r.Context(), now.Add(-time.Duration(hours)*time.Hour).UnixMilli())
	if err != nil {
		n.fail(w, err)
		return
	}
	health := SummarizeHealth(calls, now, hours)
	cut := now.Add(-healthWindow).UnixMilli()
	start := sort.Search(len(calls), func(i int) bool { return calls[i].At >= cut })
	health.LastHour.Failing, health.LastHour.Failed, health.LastHour.Calls = Failing(calls[start:])
	part.JSON(w, http.StatusOK, health)
}

// Unrecovered is how many of the calls' failures were not made good by a retry
// without JSON mode, and how many calls that leaves.
func Unrecovered(calls []HealthCall) (failed, total int) {
	recovered := 0
	for _, c := range calls {
		switch {
		case c.Purpose == retryPurpose && c.Error == "":
			recovered++
		case c.Purpose == retryPurpose:
			// The retry failed too: its first attempt already counts.
		case c.Error != "":
			failed++
			total++
		default:
			total++
		}
	}
	failed -= min(recovered, failed)
	return failed, total
}

// Failing reports whether the calls show a model that fails more than it
// answers, by the health loop's thresholds.
func Failing(calls []HealthCall) (bool, int, int) {
	failed, total := Unrecovered(calls)
	return failed >= healthMinFailed && float64(failed) >= healthFailShare*float64(total), failed, total
}

// checkHealth raises the alert while the model is failing and resolves it
// once it is not. Resolving is sent once, after a raise or at start.
func (n *Norn) checkHealth(ctx context.Context) error {
	calls, err := n.Store.HealthCalls(ctx, time.Now().Add(-healthWindow).UnixMilli())
	if err != nil {
		return err
	}
	failing, failed, total := Failing(calls)
	// Still fine: nothing to say. Still failing: raised again, with the new count.
	if n.modelChecked && !failing && !n.modelFailing {
		return nil
	}
	var sent error
	if failing {
		sent = n.Gjallar.Raise(ctx, alert.Raise{
			Key: healthAlertKey, Severity: alert.Warning,
			Title:  "The local model is failing: " + strconv.Itoa(failed) + " of " + strconv.Itoa(total) + " calls in the last hour",
			Detail: "See which calls and why on the System page.", Link: "/dashboard/system",
		})
	} else {
		sent = n.Gjallar.Resolve(ctx, healthAlertKey)
	}
	if sent != nil && !errors.Is(sent, alert.ErrOff) {
		return sent
	}
	n.modelFailing, n.modelChecked = failing, true
	return nil
}
