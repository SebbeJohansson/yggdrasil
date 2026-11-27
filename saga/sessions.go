package saga

// Coding sessions on every device. Each machine that runs Sindri and Sága
// (the desktop, another laptop) sends what its coding agents are doing to
// Mímir as `sindri.sessions`, read from its own Sindri. Every other device
// (the desktop's dashboard, the phone) reads them back, so a laptop does not
// have to be up when you look.
//
// Doing something to another device's session (type a prompt, answer its
// dialog, resume it, or start, follow up and bring home a cloud session) is
// a request, `sindri.session.*` or `sindri.cloud.*`, that only the device
// named in `origin` carries out, through its own Sindri, and answers with
// `sindri.request.done`. A device carries out requests only from the devices
// in its SAGA_DRIVE_FROM; with none, its sessions are there to look at only.
//
// What each event means is in Mímir's docs/events.md.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"yggdrasil/internal/part"
)

// Event kinds for sessions (docs/events.md in Mímir).
const (
	kindSessions      = "sindri.sessions"
	kindPrompt        = "sindri.session.prompt"
	kindChoose        = "sindri.session.choose"
	kindResume        = "sindri.session.resume"
	kindStart         = "sindri.session.start"
	kindCloudStart    = "sindri.cloud.start"
	kindCloudMessage  = "sindri.cloud.message"
	kindCloudTeleport = "sindri.cloud.teleport"
	kindDone          = "sindri.request.done"
)

// requestKinds are what one device may ask of another's sessions.
var requestKinds = []string{kindPrompt, kindChoose, kindResume, kindStart, kindCloudStart, kindCloudMessage, kindCloudTeleport}

const (
	// sessionsBeat: the sessions go again this often even when nothing
	// changed, so the other devices know this one is still there.
	sessionsBeat = 5 * time.Minute
	// requestTTL: a request older than this when it gets here is not carried
	// out. A prompt typed an evening ago is not what you want now.
	requestTTL = 30 * time.Minute
	// lastTextMax is how much of Claude's last words go to Mímir.
	lastTextMax = 600
)

// RemoteSession is one coding session as it goes to Mímir. Times are epoch millis.
type RemoteSession struct {
	// ID is Claude Code's session id, or herdr:<pane> for a pane with no Claude session.
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Directory string `json:"directory,omitempty"`
	StartedAt *int64 `json:"startedAt,omitempty"`
	// LastActivity is when its transcript last changed.
	LastActivity int64 `json:"lastActivity"`
	// Phase is busy, turn-ended or blocked (a dialog waits for you).
	Phase    string `json:"phase,omitempty"`
	Tool     string `json:"tool,omitempty"`
	LastText string `json:"lastText,omitempty"`
	// Permission is the tool a permission prompt waits on.
	Permission string `json:"permission,omitempty"`
	// BridgeSessionID is set when Remote Control is on: claude.ai/code/<it> opens it.
	BridgeSessionID string `json:"bridgeSessionId,omitempty"`
	// Pane is its herdr pane: only a session in one can be typed into.
	Pane   string        `json:"pane,omitempty"`
	Dialog *RemoteDialog `json:"dialog,omitempty"`
}

// RemoteDialog is the dialog a herdr pane shows, to answer from elsewhere.
type RemoteDialog struct {
	Question string         `json:"question"`
	Detail   string         `json:"detail,omitempty"`
	Choices  []RemoteChoice `json:"choices"`
}

// RemoteChoice is one of a dialog's choices.
type RemoteChoice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// RemoteCloud is a cloud session the device's Sindri started.
type RemoteCloud struct {
	ID           string `json:"id"`
	At           int64  `json:"at"`
	Cwd          string `json:"cwd"`
	Task         string `json:"task"`
	URL          string `json:"url"`
	LastSentAt   *int64 `json:"lastSentAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	TeleportedAt *int64 `json:"teleportedAt,omitempty"`
}

// RemoteProject is a repo on the device, to start a cloud session from.
type RemoteProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DeviceSessions is the body of `sindri.sessions`: everything at once, and
// the newest one from a device is the truth.
type DeviceSessions struct {
	At       int64           `json:"at"`
	Sessions []RemoteSession `json:"sessions"`
	Cloud    []RemoteCloud   `json:"cloud"`
	Projects []RemoteProject `json:"projects"`
	// DriveFrom is the devices whose requests this one carries out.
	DriveFrom []string `json:"driveFrom"`
	// Herdr says whether sessions here can be typed into, resumed and brought home.
	Herdr bool `json:"herdr"`
	// Errors says what could not be read, by source.
	Errors map[string]string `json:"errors,omitempty"`
}

func (d DeviceSessions) fingerprint() string {
	d.At = 0
	for i := range d.Sessions {
		d.Sessions[i].LastActivity = 0
	}
	data, _ := json.Marshal(d)
	return string(data)
}

// SessionRequest is the body of a request. Origin is the device that owns the
// session; only it carries the request out.
type SessionRequest struct {
	ID      string `json:"id"`
	Origin  string `json:"origin"`
	Session string `json:"session,omitempty"`
	Text    string `json:"text,omitempty"`
	// Question, Key and Label answer a dialog, as it was read.
	Question string `json:"question,omitempty"`
	Key      string `json:"key,omitempty"`
	Label    string `json:"label,omitempty"`
	// Cwd is where to resume or start, on the origin device.
	Cwd  string `json:"cwd,omitempty"`
	Task string `json:"task,omitempty"`
}

// RequestDone is the body of `sindri.request.done`, sent by the origin to the
// device that asked (To).
type RequestDone struct {
	ID    string `json:"id"`
	To    string `json:"to"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// URL is a new cloud session's link, when one was started.
	URL string `json:"url,omitempty"`
}

// --- reading this machine's Sindri ------------------------------------------------

type sindriSessions struct {
	Claude struct {
		Sessions []struct {
			SessionID       string `json:"sessionId"`
			PID             int    `json:"pid"`
			Directory       string `json:"directory"`
			Name            string `json:"name"`
			StartedAt       *int64 `json:"startedAt"`
			BridgeSessionID string `json:"bridgeSessionId"`
			LastActivity    int64  `json:"lastActivity"`
			State           *struct {
				Phase    string `json:"phase"`
				Tool     string `json:"tool"`
				LastText string `json:"lastText"`
			} `json:"state"`
			Permission *struct {
				Tool string `json:"tool"`
			} `json:"permission"`
		} `json:"sessions"`
		Error string `json:"error"`
	} `json:"claude"`
	Herdr struct {
		Sessions []struct {
			PaneID       string `json:"paneId"`
			Name         string `json:"name"`
			Kind         string `json:"kind"`
			Status       string `json:"status"`
			Cwd          string `json:"cwd"`
			Title        string `json:"title"`
			SessionID    string `json:"sessionId"`
			PIDs         []int  `json:"pids"`
			Dialog       string `json:"dialog"`
			DialogScreen *struct {
				Question string `json:"question"`
				Detail   string `json:"detail"`
				Choices  []struct {
					Key   string `json:"key"`
					Label string `json:"label"`
				} `json:"choices"`
			} `json:"dialogScreen"`
		} `json:"sessions"`
		Error string `json:"error"`
		From  string `json:"from"`
	} `json:"herdr"`
}

// readSessions is this machine's sessions as they go to Mímir.
func (s *Saga) readSessions(ctx context.Context) (DeviceSessions, error) {
	out := DeviceSessions{At: s.now().UnixMilli(), Sessions: []RemoteSession{}, Cloud: []RemoteCloud{}, Projects: []RemoteProject{},
		DriveFrom: s.DriveFrom, Errors: map[string]string{}}
	if out.DriveFrom == nil {
		out.DriveFrom = []string{}
	}
	var view sindriSessions
	if err := s.Sindri.Do(ctx, http.MethodGet, "/v1/sessions", nil, &view); err != nil {
		return out, err
	}
	out.Herdr = view.Herdr.From != "off" && view.Herdr.Error == ""
	if view.Claude.Error != "" {
		out.Errors["claude"] = view.Claude.Error
	}
	if view.Herdr.Error != "" && view.Herdr.From != "off" {
		out.Errors["herdr"] = view.Herdr.Error
	}
	claimed := map[string]bool{}
	for _, c := range view.Claude.Sessions {
		session := RemoteSession{ID: c.SessionID, Kind: "claude", Name: c.Name, Directory: c.Directory, StartedAt: c.StartedAt,
			LastActivity: c.LastActivity, BridgeSessionID: c.BridgeSessionID}
		if c.State != nil {
			session.Phase, session.Tool, session.LastText = c.State.Phase, c.State.Tool, clip(c.State.LastText, lastTextMax)
		}
		if c.Permission != nil {
			session.Permission = c.Permission.Tool
			if session.Permission == "" {
				session.Permission = "a tool"
			}
		}
		for _, h := range view.Herdr.Sessions {
			if claimed[h.PaneID] || !(h.SessionID == c.SessionID || slices.Contains(h.PIDs, c.PID)) {
				continue
			}
			claimed[h.PaneID] = true
			session.Pane = h.PaneID
			addDialog(&session, h.Status, h.DialogScreen)
			break
		}
		out.Sessions = append(out.Sessions, session)
	}
	for _, h := range view.Herdr.Sessions {
		if claimed[h.PaneID] {
			continue
		}
		name := h.Name
		if name == "" {
			name = h.Title
		}
		session := RemoteSession{ID: "herdr:" + h.PaneID, Kind: h.Kind, Name: name, Directory: h.Cwd, Pane: h.PaneID, Phase: herdrPhase(h.Status)}
		addDialog(&session, h.Status, h.DialogScreen)
		out.Sessions = append(out.Sessions, session)
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].LastActivity > out.Sessions[j].LastActivity })

	var cloud struct {
		Sessions []RemoteCloud `json:"sessions"`
		Off      string        `json:"off"`
	}
	if err := s.Sindri.Do(ctx, http.MethodGet, "/v1/cloud", nil, &cloud); err != nil {
		out.Errors["cloud"] = err.Error()
	} else if cloud.Off != "" {
		out.Errors["cloud"] = cloud.Off
	} else if cloud.Sessions != nil {
		out.Cloud = cloud.Sessions
	}
	var projects []RemoteProject
	if err := s.Sindri.Do(ctx, http.MethodGet, "/v1/projects", nil, &projects); err != nil {
		out.Errors["projects"] = err.Error()
	} else if projects != nil {
		out.Projects = projects
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out, nil
}

func herdrPhase(status string) string {
	switch status {
	case "working":
		return "busy"
	case "blocked":
		return "blocked"
	case "idle", "done":
		return "turn-ended"
	}
	return ""
}

func addDialog(session *RemoteSession, status string, screen *struct {
	Question string `json:"question"`
	Detail   string `json:"detail"`
	Choices  []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	} `json:"choices"`
}) {
	if status != "blocked" || screen == nil {
		return
	}
	session.Phase = "blocked"
	dialog := &RemoteDialog{Question: clip(screen.Question, 300), Detail: clip(screen.Detail, 600), Choices: []RemoteChoice{}}
	for _, c := range screen.Choices {
		dialog.Choices = append(dialog.Choices, RemoteChoice{Key: c.Key, Label: clip(c.Label, 120)})
	}
	session.Dialog = dialog
}

// scanSessions sends this machine's sessions when they changed, or when the
// beat is due.
func (s *Saga) scanSessions(ctx context.Context) error {
	if !s.Sindri.On() {
		return nil
	}
	sessions, err := s.readSessions(ctx)
	if err != nil {
		return err
	}
	fingerprint := sessions.fingerprint()
	now := s.now()
	return s.Store.tx(ctx, func(tx *sql.Tx) error {
		was, at, err := sessionsPublished(ctx, tx)
		if err != nil {
			return err
		}
		if was == fingerprint && now.Sub(time.UnixMilli(at)) < sessionsBeat {
			return nil
		}
		if err := enqueue(ctx, tx, now.UnixMilli(), kindSessions, sessions); err != nil {
			return err
		}
		return setSessionsPublished(ctx, tx, fingerprint, now.UnixMilli())
	})
}

// --- what comes back from Mímir ----------------------------------------------------

// applySessions keeps each other device's newest sessions, takes in the
// requests made of this machine, and the answers to the ones it made.
func (s *Saga) applySessions(ctx context.Context, events []Event) error {
	return s.Store.tx(ctx, func(tx *sql.Tx) error {
		for _, e := range events {
			switch {
			case e.Kind == kindSessions:
				var body DeviceSessions
				if json.Unmarshal(e.Body, &body) != nil {
					continue
				}
				if err := setDeviceSessions(ctx, tx, e.Device, e.At, string(e.Body)); err != nil {
					return err
				}
			case slices.Contains(requestKinds, e.Kind):
				var r SessionRequest
				if json.Unmarshal(e.Body, &r) != nil || r.ID == "" || r.Origin != s.Device {
					continue
				}
				if err := takeRequest(ctx, tx, e.Device, e.At, e.Kind, string(e.Body), r.ID); err != nil {
					return err
				}
			case e.Kind == kindDone:
				var d RequestDone
				if json.Unmarshal(e.Body, &d) != nil || d.To != s.Device {
					continue
				}
				if err := answerAsked(ctx, tx, d, e.At); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// --- carrying out what other devices asked ------------------------------------------

func (s *Saga) mayDrive(device string) bool { return slices.Contains(s.DriveFrom, device) }

// RunRequests carries out the requests that wait, one at a time, and answers
// each. A request is carried out once: Sindri not answering leaves it to try
// again, any answer from Sindri is the answer.
func (s *Saga) RunRequests(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	waiting, err := s.Store.waitingRequests(ctx)
	if err != nil {
		return err
	}
	ran := false
	for _, w := range waiting {
		var r SessionRequest
		_ = json.Unmarshal([]byte(w.body), &r)
		done := RequestDone{ID: r.ID, To: w.from}
		switch {
		case !s.mayDrive(w.from):
			done.Error = fmt.Sprintf("%s does not take requests from %s (SAGA_DRIVE_FROM)", s.Device, w.from)
		case s.now().Sub(time.UnixMilli(w.at)) > requestTTL:
			done.Error = fmt.Sprintf("%s was not on to do it within %s", s.Device, requestTTL)
		default:
			url, err := s.carryOut(ctx, w.kind, r)
			if err != nil && !strings.Contains(err.Error(), " answered ") {
				return err // Sindri is not answering: try again on the next round
			}
			done.OK, done.URL = err == nil, url
			if err != nil {
				done.Error = sindriText(err)
			}
		}
		if err := s.Store.tx(ctx, func(tx *sql.Tx) error {
			if err := enqueue(ctx, tx, s.now().UnixMilli(), kindDone, done); err != nil {
				return err
			}
			return finishRequest(ctx, tx, r.ID)
		}); err != nil {
			return err
		}
		ran = true
	}
	if ran {
		s.nudge()
	}
	return nil
}

// sindriText is Sindri's own words from an error, without the HTTP around them.
func sindriText(err error) string {
	text := err.Error()
	if _, after, ok := strings.Cut(text, ": "); ok && strings.Contains(text, " answered ") {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(after), &body) == nil && body.Error != "" {
			return body.Error
		}
		return after
	}
	return text
}

// carryOut does one request in this machine's Sindri.
func (s *Saga) carryOut(ctx context.Context, kind string, r SessionRequest) (string, error) {
	pane := func() (string, error) {
		sessions, err := s.readSessions(ctx)
		if err != nil {
			return "", err
		}
		for _, session := range sessions.Sessions {
			if session.ID == r.Session {
				if session.Pane == "" {
					return "", fmt.Errorf("sindri answered 409: this session is not in a herdr pane on %s, so it can't be typed into from here; open it with Remote Control instead", s.Device)
				}
				return session.Pane, nil
			}
		}
		return "", fmt.Errorf("sindri answered 404: the session is no longer running on %s", s.Device)
	}
	path := func(p string, args ...any) string {
		escaped := make([]any, len(args))
		for i, a := range args {
			escaped[i] = url.PathEscape(fmt.Sprint(a))
		}
		return fmt.Sprintf(p, escaped...)
	}
	switch kind {
	case kindPrompt:
		target, err := pane()
		if err != nil {
			return "", err
		}
		return "", s.Sindri.Do(ctx, http.MethodPost, path("/v1/sessions/herdr/%s/prompt", target), map[string]string{"text": r.Text}, nil)
	case kindChoose:
		target, err := pane()
		if err != nil {
			return "", err
		}
		return "", s.Sindri.Do(ctx, http.MethodPost, path("/v1/sessions/herdr/%s/choose", target),
			map[string]string{"question": r.Question, "key": r.Key, "label": r.Label}, nil)
	case kindResume:
		return "", s.Sindri.Do(ctx, http.MethodPost, "/v1/sessions/herdr", map[string]string{"kind": "claude", "cwd": r.Cwd, "resume": r.Session}, nil)
	case kindStart:
		// Claude on this machine, in a herdr pane in the project, starting on the task.
		return "", s.Sindri.Do(ctx, http.MethodPost, "/v1/sessions/herdr", map[string]string{"kind": "claude", "cwd": r.Cwd, "prompt": r.Task}, nil)
	case kindCloudStart:
		var started struct {
			Session RemoteCloud `json:"session"`
		}
		err := s.Sindri.Do(ctx, http.MethodPost, "/v1/cloud", map[string]string{"cwd": r.Cwd, "task": r.Task}, &started)
		return started.Session.URL, err
	case kindCloudMessage:
		return "", s.Sindri.Do(ctx, http.MethodPost, path("/v1/cloud/%s/message", r.Session), map[string]string{"text": r.Text}, nil)
	case kindCloudTeleport:
		return "", s.Sindri.Do(ctx, http.MethodPost, path("/v1/cloud/%s/teleport", r.Session), map[string]any{}, nil)
	}
	return "", fmt.Errorf("saga answered 400: %s is not a request Sága knows", kind)
}

// --- routes --------------------------------------------------------------------------

// DeviceView is one other device's sessions, as last heard.
type DeviceView struct {
	Device string `json:"device"`
	// HeardAt is when the device last sent its sessions, epoch millis.
	HeardAt  int64          `json:"heardAt"`
	Sessions DeviceSessions `json:"sessions"`
	// Drivable says whether this machine's requests will be carried out there.
	Drivable bool `json:"drivable"`
}

// Asked is a request this machine made, and how it went.
type Asked struct {
	ID      string         `json:"id"`
	Device  string         `json:"device"`
	Kind    string         `json:"kind"`
	Request SessionRequest `json:"request"`
	At      int64          `json:"at"`
	// State is waiting (not answered yet), done or failed.
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
	URL    string `json:"url,omitempty"`
	DoneAt *int64 `json:"doneAt,omitempty"`
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Saga) sessionsView(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.devices(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range devices {
		devices[i].Drivable = slices.Contains(devices[i].Sessions.DriveFrom, s.Device)
	}
	asked, err := s.Store.asked(r.Context(), s.now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"device": s.Device, "devices": devices, "asked": asked})
}

// ask is POST /v1/sessions/{device}/requests: the owner asks a thing of another
// device's session; it goes the next time this machine syncs.
func (s *Saga) ask(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may do this")
		return
	}
	device := r.PathValue("device")
	if device == s.Device {
		part.Error(w, http.StatusBadRequest, "that is this machine: ask its Sindri directly")
		return
	}
	var body struct {
		Kind string `json:"kind"`
		SessionRequest
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	kind := body.Kind
	if !strings.HasPrefix(kind, "sindri.") {
		kind = "sindri." + kind
	}
	if !slices.Contains(requestKinds, kind) {
		part.Error(w, http.StatusBadRequest, "`kind` must be one of "+strings.Join(requestKinds, ", "))
		return
	}
	req := body.SessionRequest
	req.ID, req.Origin = newID(), device
	need := map[string][]string{
		kindPrompt: {"session", "text"}, kindChoose: {"session", "question", "key", "label"}, kindResume: {"session", "cwd"},
		kindStart: {"cwd", "task"}, kindCloudStart: {"cwd", "task"}, kindCloudMessage: {"session", "text"}, kindCloudTeleport: {"session"},
	}
	values := map[string]string{"session": req.Session, "text": req.Text, "question": req.Question, "key": req.Key, "label": req.Label, "cwd": req.Cwd, "task": req.Task}
	for _, field := range need[kind] {
		if strings.TrimSpace(values[field]) == "" {
			part.Error(w, http.StatusBadRequest, fmt.Sprintf("`%s` is needed for %s", field, kind))
			return
		}
	}
	now := s.now().UnixMilli()
	err := s.Store.tx(r.Context(), func(tx *sql.Tx) error {
		if err := enqueue(r.Context(), tx, now, kind, req); err != nil {
			return err
		}
		return addAsked(r.Context(), tx, req, kind, now)
	})
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.nudge()
	part.JSON(w, http.StatusAccepted, map[string]any{"asked": Asked{ID: req.ID, Device: device, Kind: kind, Request: req, At: now, State: "waiting"}})
}

// --- store ---------------------------------------------------------------------------

const sessionsSchema = `
CREATE TABLE IF NOT EXISTS device_sessions (device TEXT PRIMARY KEY, at INTEGER NOT NULL, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions_published (id INTEGER PRIMARY KEY CHECK (id = 1), fingerprint TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS taken_requests (
	id TEXT PRIMARY KEY, device TEXT NOT NULL, at INTEGER NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS asked (
	id TEXT PRIMARY KEY, device TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, at INTEGER NOT NULL,
	done_at INTEGER, ok INTEGER, error TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT ''
);
`

func sessionsPublished(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	var fingerprint string
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT fingerprint, at FROM sessions_published WHERE id = 1`).Scan(&fingerprint, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return fingerprint, at, err
}

func setSessionsPublished(ctx context.Context, tx *sql.Tx, fingerprint string, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions_published (id, fingerprint, at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET fingerprint = excluded.fingerprint, at = excluded.at`, fingerprint, at)
	return err
}

func setDeviceSessions(ctx context.Context, tx *sql.Tx, device string, at int64, body string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO device_sessions (device, at, body) VALUES (?, ?, ?)
		ON CONFLICT (device) DO UPDATE SET at = excluded.at, body = excluded.body WHERE excluded.at >= device_sessions.at`, device, at, body)
	return err
}

func (s *Store) devices(ctx context.Context) ([]DeviceView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device, at, body FROM device_sessions ORDER BY device`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceView{}
	for rows.Next() {
		var d DeviceView
		var body string
		if err := rows.Scan(&d.Device, &d.HeardAt, &body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &d.Sessions) != nil {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func takeRequest(ctx context.Context, tx *sql.Tx, device string, at int64, kind, body, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO taken_requests (id, device, at, kind, body) VALUES (?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		id, device, at, kind, body)
	return err
}

type waitingRequest struct {
	from, kind, body string
	at               int64
}

func (s *Store) waitingRequests(ctx context.Context) ([]waitingRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device, at, kind, body FROM taken_requests WHERE done = 0 ORDER BY at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []waitingRequest
	for rows.Next() {
		var w waitingRequest
		if err := rows.Scan(&w.from, &w.at, &w.kind, &w.body); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func finishRequest(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE taken_requests SET done = 1 WHERE id = ?`, id)
	return err
}

func addAsked(ctx context.Context, tx *sql.Tx, r SessionRequest, kind string, at int64) error {
	body, _ := json.Marshal(r)
	_, err := tx.ExecContext(ctx, `INSERT INTO asked (id, device, kind, body, at) VALUES (?, ?, ?, ?, ?)`, r.ID, r.Origin, kind, string(body), at)
	return err
}

func answerAsked(ctx context.Context, tx *sql.Tx, d RequestDone, at int64) error {
	ok := 0
	if d.OK {
		ok = 1
	}
	_, err := tx.ExecContext(ctx, `UPDATE asked SET done_at = ?, ok = ?, error = ?, url = ? WHERE id = ?`, at, ok, d.Error, d.URL, d.ID)
	return err
}

func (s *Store) asked(ctx context.Context, since int64) ([]Asked, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, device, kind, body, at, done_at, ok, error, url FROM asked WHERE at >= ? ORDER BY at DESC LIMIT 100`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Asked{}
	for rows.Next() {
		var a Asked
		var body string
		var doneAt, ok sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Device, &a.Kind, &body, &a.At, &doneAt, &ok, &a.Error, &a.URL); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(body), &a.Request)
		a.State = "waiting"
		if doneAt.Valid {
			a.DoneAt = &doneAt.Int64
			a.State = "failed"
			if ok.Int64 == 1 {
				a.State = "done"
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
