package norn

// The Microsoft and Google sign-ins, held here so the widget's window and the
// dashboard share them. Ported from my-agent's microsoft/auth.ts and
// google/auth.ts.
//
// Microsoft: device code. You are shown a code, type it into
// microsoft.com/devicelogin once, and the refresh token that comes back is
// good for weeks. The client is Microsoft's own public "Graph Command Line
// Tools", so nothing is registered in the directory. Microsoft rotates the
// refresh token on every use, so the new one is saved each time.
//
// Google: a "Desktop app" OAuth client of your own, with the loopback flow
// and PKCE: Google has no public client to borrow, and its device flow does
// not allow the Gmail or Calendar scopes. The consent page sends the browser
// back to http://localhost:42814/google, which Norn listens on for that one
// redirect.
//
// Tokens are kept in files beside the database, readable by you only.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrSignedOut is returned when there is no sign-in, or the provider ended it.
var ErrSignedOut = errors.New("not signed in")

// refreshSlack: a token this close to expiring is refreshed first.
const refreshSlack = time.Minute

// formPost is one urlencoded POST to a token endpoint. Errors come back as
// JSON with a 400, so the body is read whatever the status.
type formPost func(ctx context.Context, endpoint string, fields url.Values) (map[string]any, error)

func defaultFormPost(ctx context.Context, endpoint string, fields url.Values) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(fields.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	return body, nil
}

func text(body map[string]any, key string) string {
	if value, ok := body[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

func seconds(body map[string]any, key string, fallback float64) time.Duration {
	if value, ok := body[key].(float64); ok && value > 0 {
		return time.Duration(value * float64(time.Second))
	}
	return time.Duration(fallback * float64(time.Second))
}

func reason(body map[string]any) string {
	why := text(body, "error_description")
	if why == "" {
		why = text(body, "error")
	}
	if why == "" {
		why = "no reason given"
	}
	why = strings.SplitN(why, "\n", 2)[0]
	if len(why) > 300 {
		why = why[:300]
	}
	return why
}

func saveJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// --- Microsoft -----------------------------------------------------------------

// GraphCLIClientID is Microsoft's "Graph Command Line Tools" public client.
const GraphCLIClientID = "14d82eec-204b-4c2f-b7e8-296a70dab67e"

// MicrosoftScopes are the optional parts of Microsoft 365 a sign-in asks for.
type MicrosoftScopes struct {
	Mail     bool `json:"mail"`
	Calendar bool `json:"calendar"`
}

// ScopesFor is the scope string: offline_access makes the refresh token
// appear, Chat.Read is Teams, User.Read.All is the org chart senders are
// ranked by. Mail and calendar only when asked for: a scope you ask for is
// one the tenant may make you chase an admin over.
func ScopesFor(scopes MicrosoftScopes) string {
	list := []string{"offline_access", "User.Read", "User.Read.All", "Chat.Read"}
	if scopes.Mail {
		list = append(list, "Mail.Read")
	}
	if scopes.Calendar {
		list = append(list, "Calendars.Read")
	}
	return strings.Join(list, " ")
}

// MicrosoftCode is what to type, where, and until when. Never the device code itself.
type MicrosoftCode struct {
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresAt       int64  `json:"expiresAt"`
}

// MicrosoftStatus is the sign-in as the dashboard shows it.
type MicrosoftStatus struct {
	State  string           `json:"state"` // signed-out, linking, signed-in, error
	Code   *MicrosoftCode   `json:"code,omitempty"`
	Error  string           `json:"error,omitempty"`
	Scopes *MicrosoftScopes `json:"scopes,omitempty"`
}

// AccessToken is a usable token and when it expires (epoch ms).
type AccessToken struct {
	AccessToken string `json:"accessToken"`
	ExpiresAt   int64  `json:"expiresAt"`
}

type microsoftStored struct {
	Tenant       string          `json:"tenant"`
	Scopes       MicrosoftScopes `json:"scopes"`
	RefreshToken string          `json:"refreshToken"`
	AccessToken  string          `json:"accessToken,omitempty"`
	ExpiresAt    int64           `json:"expiresAt,omitempty"`
}

type microsoftLinking struct {
	tenant     string
	scopes     MicrosoftScopes
	deviceCode string
	code       MicrosoftCode
	interval   time.Duration
}

// MicrosoftAuth holds the Microsoft sign-in.
type MicrosoftAuth struct {
	file string
	post formPost
	now  func() time.Time
	// OnChange is called when a sign-in completes (nil) or ends with a reason.
	OnChange func(err error)

	mu         sync.Mutex
	stored     *microsoftStored
	linking    *microsoftLinking
	err        string
	refreshing chan struct{}
	pollStop   chan struct{}
}

// NewMicrosoftAuth reads the sign-in kept at file, if there is one.
func NewMicrosoftAuth(file string) *MicrosoftAuth {
	a := &MicrosoftAuth{file: file, post: defaultFormPost, now: time.Now}
	if data, err := os.ReadFile(file); err == nil {
		var stored microsoftStored
		if json.Unmarshal(data, &stored) == nil && stored.RefreshToken != "" {
			a.stored = &stored
		}
	}
	return a
}

func microsoftAuthority(tenant string) string {
	if tenant == "" {
		tenant = "organizations"
	}
	return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0"
}

// Status is the sign-in now.
func (a *MicrosoftAuth) Status() MicrosoftStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

func (a *MicrosoftAuth) statusLocked() MicrosoftStatus {
	if a.linking != nil && a.linking.code.ExpiresAt > a.now().UnixMilli() {
		code, scopes := a.linking.code, a.linking.scopes
		return MicrosoftStatus{State: "linking", Code: &code, Scopes: &scopes}
	}
	if a.stored != nil {
		scopes := a.stored.Scopes
		return MicrosoftStatus{State: "signed-in", Scopes: &scopes}
	}
	if a.err != "" {
		return MicrosoftStatus{State: "error", Error: a.err}
	}
	return MicrosoftStatus{State: "signed-out"}
}

// SignIn starts a sign-in, or hands back the one waiting for its code when it
// asks for the same scopes, or nothing when the sign-in there covers them,
// unless force. It polls Microsoft in the background until the code is
// entered or runs out.
func (a *MicrosoftAuth) SignIn(ctx context.Context, tenant string, scopes MicrosoftScopes, force bool) MicrosoftStatus {
	a.mu.Lock()
	if held := a.stored; !force && held != nil && (held.Scopes.Mail || !scopes.Mail) && (held.Scopes.Calendar || !scopes.Calendar) {
		defer a.mu.Unlock()
		return a.statusLocked()
	}
	if waiting := a.linking; waiting != nil && waiting.code.ExpiresAt > a.now().UnixMilli() && waiting.tenant == tenant && waiting.scopes == scopes {
		defer a.mu.Unlock()
		return a.statusLocked()
	}
	if a.pollStop != nil {
		close(a.pollStop)
		a.pollStop = nil
	}
	a.mu.Unlock()

	body, err := a.post(ctx, microsoftAuthority(tenant)+"/devicecode", url.Values{"client_id": {GraphCLIClientID}, "scope": {ScopesFor(scopes)}})
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil || text(body, "device_code") == "" {
		a.linking = nil
		why := reason(body)
		if err != nil {
			why = err.Error()
		}
		a.err = "Microsoft would not start the sign-in: " + why
		return a.statusLocked()
	}
	a.err = ""
	verification := text(body, "verification_uri")
	if verification == "" {
		verification = "https://microsoft.com/devicelogin"
	}
	interval := seconds(body, "interval", 5)
	a.linking = &microsoftLinking{
		tenant: tenant, scopes: scopes, deviceCode: text(body, "device_code"), interval: interval,
		code: MicrosoftCode{UserCode: text(body, "user_code"), VerificationURI: verification, ExpiresAt: a.now().Add(seconds(body, "expires_in", 900)).UnixMilli()},
	}
	stop := make(chan struct{})
	a.pollStop = stop
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(interval):
			}
			if !a.Poll(context.Background()) {
				return
			}
		}
	}()
	return a.statusLocked()
}

// Poll asks once whether the code was entered; true while it still waits.
func (a *MicrosoftAuth) Poll(ctx context.Context) bool {
	a.mu.Lock()
	linking := a.linking
	if linking == nil {
		a.mu.Unlock()
		return false
	}
	if linking.code.ExpiresAt <= a.now().UnixMilli() {
		a.linking = nil
		a.mu.Unlock()
		return false
	}
	a.mu.Unlock()
	body, err := a.post(ctx, microsoftAuthority(linking.tenant)+"/token", url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {GraphCLIClientID},
		"device_code": {linking.deviceCode},
	})
	if err != nil {
		// The network blinked; the code is still good.
		return true
	}
	a.mu.Lock()
	if text(body, "refresh_token") != "" {
		a.linking = nil
		a.saveLocked(&microsoftStored{
			Tenant: linking.tenant, Scopes: linking.scopes, RefreshToken: text(body, "refresh_token"),
			AccessToken: text(body, "access_token"), ExpiresAt: a.now().Add(seconds(body, "expires_in", 3600)).UnixMilli(),
		})
		onChange := a.OnChange
		a.mu.Unlock()
		if onChange != nil {
			onChange(nil)
		}
		return false
	}
	defer a.mu.Unlock()
	if e := text(body, "error"); e == "authorization_pending" || e == "slow_down" {
		return true
	}
	a.linking = nil
	a.err = "The sign-in failed: " + reason(body)
	return false
}

// Token is a usable access token, refreshed when close to expiring. Callers
// at once share one refresh: each refresh rotates the refresh token, and a
// second one would spend the first's.
func (a *MicrosoftAuth) Token(ctx context.Context) (AccessToken, error) {
	for {
		a.mu.Lock()
		stored := a.stored
		if stored == nil {
			a.mu.Unlock()
			return AccessToken{}, ErrSignedOut
		}
		if stored.AccessToken != "" && time.UnixMilli(stored.ExpiresAt).Add(-refreshSlack).After(a.now()) {
			a.mu.Unlock()
			return AccessToken{AccessToken: stored.AccessToken, ExpiresAt: stored.ExpiresAt}, nil
		}
		if waiting := a.refreshing; waiting != nil {
			a.mu.Unlock()
			<-waiting
			continue
		}
		done := make(chan struct{})
		a.refreshing = done
		a.mu.Unlock()
		token, err := a.refresh(ctx, stored)
		a.mu.Lock()
		a.refreshing = nil
		a.mu.Unlock()
		close(done)
		return token, err
	}
}

func (a *MicrosoftAuth) refresh(ctx context.Context, stored *microsoftStored) (AccessToken, error) {
	body, err := a.post(ctx, microsoftAuthority(stored.Tenant)+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {GraphCLIClientID},
		"refresh_token": {stored.RefreshToken},
		"scope":         {ScopesFor(stored.Scopes)},
	})
	if err != nil {
		return AccessToken{}, fmt.Errorf("could not refresh the Microsoft sign-in: %w", err)
	}
	if text(body, "access_token") == "" {
		if text(body, "error") == "invalid_grant" {
			// Revoked, expired, or a scope never consented to: only a new sign-in fixes it.
			why := "Microsoft ended the sign-in: " + reason(body)
			a.mu.Lock()
			a.forgetLocked(why)
			onChange := a.OnChange
			a.mu.Unlock()
			if onChange != nil {
				onChange(errors.New(why))
			}
			return AccessToken{}, ErrSignedOut
		}
		return AccessToken{}, fmt.Errorf("could not refresh the Microsoft sign-in: %s", reason(body))
	}
	next := *stored
	if refresh := text(body, "refresh_token"); refresh != "" {
		next.RefreshToken = refresh
	}
	next.AccessToken = text(body, "access_token")
	next.ExpiresAt = a.now().Add(seconds(body, "expires_in", 3600)).UnixMilli()
	a.mu.Lock()
	a.saveLocked(&next)
	a.mu.Unlock()
	return AccessToken{AccessToken: next.AccessToken, ExpiresAt: next.ExpiresAt}, nil
}

// SignOut forgets the sign-in.
func (a *MicrosoftAuth) SignOut() MicrosoftStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pollStop != nil {
		close(a.pollStop)
		a.pollStop = nil
	}
	a.linking = nil
	a.forgetLocked("")
	return a.statusLocked()
}

func (a *MicrosoftAuth) saveLocked(stored *microsoftStored) {
	a.stored = stored
	a.err = ""
	_ = saveJSON(a.file, stored)
}

func (a *MicrosoftAuth) forgetLocked(why string) {
	a.stored = nil
	a.err = why
	_ = os.Remove(a.file)
}

// --- Google ----------------------------------------------------------------------

// Google's scopes, and where its consent page sends the browser back.
const (
	GmailScope          = "https://www.googleapis.com/auth/gmail.readonly"
	GoogleCalendarScope = "https://www.googleapis.com/auth/calendar.readonly"
	GoogleTasksScope    = "https://www.googleapis.com/auth/tasks.readonly"
	GoogleRedirectPort  = 42814
	googleTokenURL      = "https://oauth2.googleapis.com/token"
	googleSignInWait    = 5 * time.Minute
)

// GoogleGrants are what a sign-in covers; the consent page lets you untick any.
type GoogleGrants struct {
	Gmail    bool `json:"gmail"`
	Calendar bool `json:"calendar"`
	Tasks    bool `json:"tasks"`
}

// GrantsFrom reads the scopes Google says it granted.
func GrantsFrom(scope string) GoogleGrants {
	granted := map[string]bool{}
	for _, s := range strings.Fields(scope) {
		granted[s] = true
	}
	return GoogleGrants{Gmail: granted[GmailScope], Calendar: granted[GoogleCalendarScope], Tasks: granted[GoogleTasksScope]}
}

// GoogleStatus is the sign-in as the dashboard shows it.
type GoogleStatus struct {
	State        string        `json:"state"` // off, signed-out, waiting, signed-in, error
	AuthorizeURL string        `json:"authorizeUrl,omitempty"`
	Grants       *GoogleGrants `json:"grants,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type googleStored struct {
	RefreshToken string `json:"refreshToken"`
	Scope        string `json:"scope"`
	AccessToken  string `json:"accessToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}

type googleWaiting struct {
	verifier, state, url string
	server               *http.Server
	timer                *time.Timer
}

// GoogleAuth holds the Google sign-in.
type GoogleAuth struct {
	ClientID, ClientSecret string
	file                   string
	post                   formPost
	now                    func() time.Time
	port                   int
	// OnChange is called when a sign-in completes (nil) or ends with a reason.
	OnChange func(err error)

	mu         sync.Mutex
	stored     *googleStored
	waiting    *googleWaiting
	err        string
	refreshing chan struct{}
}

// NewGoogleAuth reads the sign-in kept at file, if there is one.
func NewGoogleAuth(file, clientID, clientSecret string) *GoogleAuth {
	a := &GoogleAuth{ClientID: clientID, ClientSecret: clientSecret, file: file, post: defaultFormPost, now: time.Now, port: GoogleRedirectPort}
	if data, err := os.ReadFile(file); err == nil {
		var stored googleStored
		if json.Unmarshal(data, &stored) == nil && stored.RefreshToken != "" {
			a.stored = &stored
		}
	}
	return a
}

// Configured is whether there is a client to sign in with at all.
func (a *GoogleAuth) Configured() bool {
	return a.ClientID != "" && a.ClientSecret != ""
}

// Status is the sign-in now.
func (a *GoogleAuth) Status() GoogleStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

func (a *GoogleAuth) statusLocked() GoogleStatus {
	switch {
	case !a.Configured():
		return GoogleStatus{State: "off", Error: "GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET are not set"}
	case a.waiting != nil:
		return GoogleStatus{State: "waiting", AuthorizeURL: a.waiting.url}
	case a.stored != nil:
		grants := GrantsFrom(a.stored.Scope)
		return GoogleStatus{State: "signed-in", Grants: &grants}
	case a.err != "":
		return GoogleStatus{State: "error", Error: a.err}
	}
	return GoogleStatus{State: "signed-out"}
}

// Grants is what the sign-in covers now; nothing when signed out.
func (a *GoogleAuth) Grants() GoogleGrants {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stored == nil {
		return GoogleGrants{}
	}
	return GrantsFrom(a.stored.Scope)
}

func randomString() string {
	buffer := make([]byte, 32)
	_, _ = rand.Read(buffer)
	return base64.RawURLEncoding.EncodeToString(buffer)
}

// PKCEPair is a verifier and its S256 challenge.
func PKCEPair() (verifier, challenge string) {
	verifier = randomString()
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:])
}

func (a *GoogleAuth) redirectURI() string {
	return fmt.Sprintf("http://localhost:%d/google", a.port)
}

// AuthorizeURL is Google's consent page. access_type=offline makes the
// refresh token appear; prompt=consent makes it appear on a second sign-in too.
func (a *GoogleAuth) AuthorizeURL(challenge, state string) string {
	params := url.Values{
		"client_id": {a.ClientID}, "redirect_uri": {a.redirectURI()}, "response_type": {"code"},
		"scope":          {strings.Join([]string{GmailScope, GoogleCalendarScope, GoogleTasksScope}, " ")},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
		"access_type": {"offline"}, "prompt": {"consent"},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()
}

// SignIn holds the redirect port and returns the consent page to open. A
// second call while one waits hands back the same page.
func (a *GoogleAuth) SignIn() (GoogleStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.Configured() || a.waiting != nil {
		return a.statusLocked(), nil
	}
	verifier, challenge := PKCEPair()
	state := randomString()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", a.port))
	if err != nil {
		return a.statusLocked(), fmt.Errorf("could not listen for Google's redirect on port %d: %w", a.port, err)
	}
	waiting := &googleWaiting{verifier: verifier, state: state, url: a.AuthorizeURL(challenge, state)}
	waiting.server = &http.Server{Handler: http.HandlerFunc(a.callback), ReadHeaderTimeout: 10 * time.Second}
	waiting.timer = time.AfterFunc(googleSignInWait, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.stopWaitingLocked("the Google sign-in timed out; start it again")
	})
	go func() { _ = waiting.server.Serve(listener) }()
	a.err = ""
	a.waiting = waiting
	return a.statusLocked(), nil
}

func (a *GoogleAuth) stopWaitingLocked(why string) {
	waiting := a.waiting
	if waiting == nil {
		return
	}
	a.waiting = nil
	waiting.timer.Stop()
	go func() { _ = waiting.server.Close() }()
	a.err = why
}

func page(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:16px system-ui;margin:15vh auto;max-width:28em;padding:0 16px;color:#cdd6f4;background:#1e1e2e"><h1 style="font-size:20px">%s</h1><p>%s</p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}

// callback is the browser coming back from Google.
func (a *GoogleAuth) callback(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	waiting := a.waiting
	a.mu.Unlock()
	if waiting == nil || r.URL.Path != "/google" {
		page(w, http.StatusNotFound, "Not here", "This only answers Google's sign-in redirect.")
		return
	}
	query := r.URL.Query()
	if query.Get("state") != waiting.state {
		page(w, http.StatusBadRequest, "Wrong sign-in", "This is not the sign-in that was started. Start it again.")
		return
	}
	code := query.Get("code")
	if code == "" {
		why := "Google refused the sign-in: " + query.Get("error")
		if query.Get("error") == "access_denied" {
			why = "the Google sign-in was cancelled"
		}
		a.mu.Lock()
		a.stopWaitingLocked(why)
		a.mu.Unlock()
		page(w, http.StatusBadRequest, "Not signed in", why)
		return
	}
	body, err := a.post(r.Context(), googleTokenURL, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {waiting.verifier},
		"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "redirect_uri": {a.redirectURI()},
	})
	if err != nil || text(body, "refresh_token") == "" || text(body, "access_token") == "" {
		why := "Google would not finish the sign-in: " + reason(body)
		a.mu.Lock()
		a.stopWaitingLocked(why)
		a.mu.Unlock()
		page(w, http.StatusBadGateway, "Not signed in", why)
		return
	}
	a.mu.Lock()
	a.stopWaitingLocked("")
	a.saveLocked(&googleStored{RefreshToken: text(body, "refresh_token"), Scope: text(body, "scope"),
		AccessToken: text(body, "access_token"), ExpiresAt: a.now().Add(seconds(body, "expires_in", 3600)).UnixMilli()})
	grants := GrantsFrom(a.stored.Scope)
	onChange := a.OnChange
	a.mu.Unlock()
	var what []string
	for _, service := range []struct {
		name    string
		granted bool
	}{{"Gmail", grants.Gmail}, {"Google Calendar", grants.Calendar}, {"Google Tasks", grants.Tasks}} {
		if service.granted {
			what = append(what, service.name)
		}
	}
	if len(what) > 0 {
		page(w, http.StatusOK, "Signed in to Google", "You can close this tab; Norn reads "+strings.Join(what, ", ")+" now.")
	} else {
		page(w, http.StatusOK, "Signed in to Google", "Signed in, but nothing was allowed. Sign in again and tick Gmail, Calendar and Tasks.")
	}
	if onChange != nil {
		onChange(nil)
	}
}

// Token is a usable access token, refreshed when close to expiring; callers
// at once share one refresh.
func (a *GoogleAuth) Token(ctx context.Context) (string, error) {
	for {
		a.mu.Lock()
		stored := a.stored
		if stored == nil {
			a.mu.Unlock()
			return "", ErrSignedOut
		}
		if stored.AccessToken != "" && time.UnixMilli(stored.ExpiresAt).Add(-refreshSlack).After(a.now()) {
			a.mu.Unlock()
			return stored.AccessToken, nil
		}
		if waiting := a.refreshing; waiting != nil {
			a.mu.Unlock()
			<-waiting
			continue
		}
		done := make(chan struct{})
		a.refreshing = done
		a.mu.Unlock()
		token, err := a.refresh(ctx, stored)
		a.mu.Lock()
		a.refreshing = nil
		a.mu.Unlock()
		close(done)
		return token, err
	}
}

func (a *GoogleAuth) refresh(ctx context.Context, stored *googleStored) (string, error) {
	body, err := a.post(ctx, googleTokenURL, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {stored.RefreshToken},
		"client_id": {a.ClientID}, "client_secret": {a.ClientSecret},
	})
	if err != nil {
		return "", fmt.Errorf("could not refresh the Google sign-in: %w", err)
	}
	if text(body, "access_token") == "" {
		if text(body, "error") == "invalid_grant" {
			// Revoked, or the seven days a "Testing" consent screen allows are up.
			why := "Google ended the sign-in: " + reason(body)
			a.mu.Lock()
			a.forgetLocked(why)
			onChange := a.OnChange
			a.mu.Unlock()
			if onChange != nil {
				onChange(errors.New(why))
			}
			return "", ErrSignedOut
		}
		return "", fmt.Errorf("could not refresh the Google sign-in: %s", reason(body))
	}
	next := *stored
	if refresh := text(body, "refresh_token"); refresh != "" {
		next.RefreshToken = refresh
	}
	if scope := text(body, "scope"); scope != "" {
		next.Scope = scope
	}
	next.AccessToken = text(body, "access_token")
	next.ExpiresAt = a.now().Add(seconds(body, "expires_in", 3600)).UnixMilli()
	a.mu.Lock()
	a.saveLocked(&next)
	a.mu.Unlock()
	return next.AccessToken, nil
}

// SignOut forgets the sign-in.
func (a *GoogleAuth) SignOut() GoogleStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopWaitingLocked("")
	a.forgetLocked("")
	return a.statusLocked()
}

func (a *GoogleAuth) saveLocked(stored *googleStored) {
	a.stored = stored
	a.err = ""
	_ = saveJSON(a.file, stored)
}

func (a *GoogleAuth) forgetLocked(why string) {
	a.stored = nil
	a.err = why
	_ = os.Remove(a.file)
}
