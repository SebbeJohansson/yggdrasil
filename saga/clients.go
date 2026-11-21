package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
)

// Mimir is the client for Mímir, the event log on the VPS
// (github.com/SebbeJohansson/mimir).
type Mimir struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Event is one event read back from Mímir.
type Event struct {
	Pos    int64           `json:"pos"`
	Device string          `json:"device"`
	Seq    int64           `json:"seq"`
	At     int64           `json:"at"`
	Kind   string          `json:"kind"`
	Body   json.RawMessage `json:"body"`
}

// errGone: Mímir no longer holds what comes after our mark, or never did
// (it was restored from a backup).
var errGone = errors.New("mimir: our mark is outside what it holds")

// gapError: Mímir holds fewer of our events than we think it does.
type gapError struct{ lastSeq int64 }

func (e *gapError) Error() string {
	return fmt.Sprintf("mimir holds our events up to %d only", e.lastSeq)
}

func (m *Mimir) on() bool { return m != nil && m.URL != "" && m.Token != "" }

func (m *Mimir) do(ctx context.Context, method, path string, body, out any) (int, error) {
	if !m.on() {
		return 0, errors.New("mimir is not set up (MIMIR_URL, MIMIR_TOKEN)")
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(m.URL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+m.Token)
	response, err := m.HTTP.Do(request)
	if err != nil {
		return 0, fmt.Errorf("mimir: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusGone {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return response.StatusCode, fmt.Errorf("mimir answered %d: %s", response.StatusCode, strings.TrimSpace(string(text)))
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			return response.StatusCode, fmt.Errorf("mimir: %w", err)
		}
	}
	return response.StatusCode, nil
}

// LastSeq is the highest seq Mímir holds from us.
func (m *Mimir) LastSeq(ctx context.Context) (int64, error) {
	var me struct {
		LastSeq int64 `json:"lastSeq"`
	}
	_, err := m.do(ctx, http.MethodGet, "/v1/me", nil, &me)
	return me.LastSeq, err
}

// Append sends events, oldest first, and says up to which seq Mímir holds ours.
func (m *Mimir) Append(ctx context.Context, events []Outgoing) (int64, error) {
	var answer struct {
		LastSeq int64 `json:"lastSeq"`
	}
	status, err := m.do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": events}, &answer)
	if err != nil {
		return 0, err
	}
	if status == http.StatusConflict {
		return 0, &gapError{lastSeq: answer.LastSeq}
	}
	return answer.LastSeq, nil
}

// Read is a page of other devices' events after since.
func (m *Mimir) Read(ctx context.Context, since int64, self string) (events []Event, last int64, more bool, err error) {
	var page struct {
		Events []Event `json:"events"`
		Last   int64   `json:"last"`
		More   bool    `json:"more"`
	}
	query := url.Values{"since": {fmt.Sprint(since)}, "from": {self}, "limit": {"500"}}
	status, err := m.do(ctx, http.MethodGet, "/v1/events?"+query.Encode(), nil, &page)
	if err != nil {
		return nil, 0, false, err
	}
	if status == http.StatusGone {
		return nil, 0, false, errGone
	}
	return page.Events, page.Last, page.More, nil
}

// SnapshotPos is where the snapshot leaves off, or 0 when there is none.
func (m *Mimir) SnapshotPos(ctx context.Context) (int64, error) {
	var snapshot struct {
		Pos int64 `json:"pos"`
	}
	_, err := m.do(ctx, http.MethodGet, "/v1/snapshot", nil, &snapshot)
	if err != nil && strings.Contains(err.Error(), "answered 404") {
		return 0, nil
	}
	return snapshot.Pos, err
}

// gAlert is an alert as Gjallar lists it.
type gAlert struct {
	ID           int64          `json:"id"`
	Source       string         `json:"source"`
	Key          string         `json:"key"`
	Severity     alert.Severity `json:"severity"`
	Title        string         `json:"title"`
	Detail       string         `json:"detail"`
	Link         string         `json:"link"`
	State        string         `json:"state"`
	FirstSeenAt  time.Time      `json:"firstSeenAt"`
	LastSeenAt   time.Time      `json:"lastSeenAt"`
	ResolvedAt   *time.Time     `json:"resolvedAt"`
	SnoozedUntil *time.Time     `json:"snoozedUntil"`
	Muted        bool           `json:"muted"`
}

// Gjallar is Gjallar as Sága uses it: as you (owner) to read and act on
// alerts, and as each other device (a sender) to raise its mirrors.
type Gjallar struct {
	Owner   *call.Part
	Devices map[string]*call.Part
}

func (g *Gjallar) list(ctx context.Context) ([]gAlert, error) {
	var answer struct {
		Alerts []gAlert `json:"alerts"`
	}
	err := g.Owner.Do(ctx, http.MethodGet, "/v1/alerts?state=all&limit=1000", nil, &answer)
	return answer.Alerts, err
}

// ignoreResolved: acknowledging or snoozing a resolved alert is a 409, and
// nothing to worry about: it is over.
func ignoreResolved(err error) error {
	if err != nil && strings.Contains(err.Error(), "answered 409") {
		return nil
	}
	return err
}

func (g *Gjallar) ack(ctx context.Context, id int64) error {
	return ignoreResolved(g.Owner.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/alerts/%d/ack", id), map[string]any{}, nil))
}

func (g *Gjallar) snooze(ctx context.Context, id int64, until time.Time) error {
	return ignoreResolved(g.Owner.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/alerts/%d/snooze", id), map[string]any{"until": until}, nil))
}

func (g *Gjallar) mute(ctx context.Context, source, key string, muted bool) error {
	return g.Owner.Do(ctx, http.MethodPost, "/v1/mutes", map[string]any{"source": source, "key": key, "muted": muted}, nil)
}

func (g *Gjallar) device(name string) (*call.Part, error) {
	p := g.Devices[name]
	if !p.On() {
		return nil, fmt.Errorf("no Gjallar token for %s in SAGA_GJALLAR_TOKENS", name)
	}
	return p, nil
}

func (g *Gjallar) raiseAs(ctx context.Context, device string, raise alert.Raise) error {
	p, err := g.device(device)
	if err != nil {
		return err
	}
	return p.Do(ctx, http.MethodPost, "/v1/alerts", raise, nil)
}

func (g *Gjallar) resolveAs(ctx context.Context, device, key string) error {
	p, err := g.device(device)
	if err != nil {
		return err
	}
	return p.Do(ctx, http.MethodPost, "/v1/alerts/resolve", map[string]string{"key": key}, nil)
}
