package norn

// The sign-in and calendar routes, the calendar loops, and the alerts for a
// sign-in that ended.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
)

// Accounts holds the sign-ins and the two calendars.
type Accounts struct {
	Microsoft *MicrosoftAuth
	Google    *GoogleAuth
	Work      *Calendar
	Personal  *Calendar
	get       bearerGet
	del       bearerDelete
}

// NewAccounts reads the sign-ins kept in dir.
func NewAccounts(dir, googleClientID, googleClientSecret string) *Accounts {
	return &Accounts{
		Microsoft: NewMicrosoftAuth(dir + "/microsoft.json"),
		Google:    NewGoogleAuth(dir+"/google.json", googleClientID, googleClientSecret),
		Work:      &Calendar{},
		Personal:  &Calendar{},
		get:       defaultBearerGet,
		del:       defaultBearerDelete,
	}
}

// signInAlert raises "the <provider> sign-in ended" on Gjallar, and resolves
// it once signed in again. Norn holds the sign-in, so Norn raises it.
func (n *Norn) signInAlert(provider, name string) func(error) {
	return func(err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		key := "signin:" + provider
		var sent error
		if err == nil {
			sent = n.Gjallar.Resolve(ctx, key)
		} else {
			sent = n.Gjallar.Raise(ctx, alert.Raise{Key: key, Severity: alert.Warning, Title: "The " + name + " sign-in ended; sign in again", Detail: err.Error()})
		}
		if sent != nil && !errors.Is(sent, alert.ErrOff) {
			n.logf("could not tell Gjallar about the %s sign-in: %v", name, sent)
		}
	}
}

func (n *Norn) mountAccounts(p *part.Part) {
	a := n.Accounts
	a.Microsoft.OnChange = n.signInAlert("microsoft", "Microsoft")
	a.Google.OnChange = func(err error) {
		n.signInAlert("google", "Google")(err)
	}
	p.Handle("GET /v1/microsoft", n.microsoftStatus)
	p.Handle("POST /v1/microsoft/sign-in", n.microsoftSignIn)
	p.Handle("POST /v1/microsoft/token", n.microsoftToken)
	p.Handle("POST /v1/microsoft/sign-out", n.microsoftSignOut)
	p.Handle("GET /v1/google", n.googleStatus)
	p.Handle("POST /v1/google/sign-in", n.googleSignIn)
	p.Handle("POST /v1/google/sign-out", n.googleSignOut)
	p.Handle("GET /v1/calendar", n.calendarRoute(a.Work))
	p.Handle("GET /v1/calendar/personal", n.calendarRoute(a.Personal))
	n.mountSuggestions(p)
	p.Loop("calendar", CalendarEvery, n.readWorkCalendar)
	p.Loop("personal-calendar", CalendarEvery, n.readPersonalCalendar)
}

func (n *Norn) hidden(ctx context.Context, feature string) bool {
	settings := WidgetSettings{Place: "unknown"}
	if _, err := n.Store.Setting(ctx, widgetSettingsKey, &settings); err != nil {
		return false
	}
	return settings.Hidden(feature)
}

func (n *Norn) workCalendarState(ctx context.Context) CalendarRead {
	status := n.Accounts.Microsoft.Status()
	if n.hidden(ctx, "outlook-calendar") || (status.State == "signed-in" && status.Scopes != nil && !status.Scopes.Calendar) {
		return CalendarRead{State: "off"}
	}
	return n.Accounts.Work.Read()
}

func (n *Norn) personalCalendarState(ctx context.Context) CalendarRead {
	if n.hidden(ctx, "google-calendar") || (n.Accounts.Google.Status().State == "signed-in" && !n.Accounts.Google.Grants().Calendar) {
		return CalendarRead{State: "off"}
	}
	return n.Accounts.Personal.Read()
}

func (n *Norn) readWorkCalendar(ctx context.Context) error {
	if n.hidden(ctx, "outlook-calendar") {
		n.Accounts.Work.Clear()
		return nil
	}
	status := n.Accounts.Microsoft.Status()
	if status.State != "signed-in" || status.Scopes == nil || !status.Scopes.Calendar {
		return nil
	}
	err := n.Accounts.Work.readInto(func() (CalendarSnapshot, error) {
		return ReadOutlook(ctx, n.Accounts.Microsoft, n.Accounts.get, time.Now())
	})
	if err == nil {
		n.calendarEvents("work", n.Accounts.Work)
		n.Events.Suggestions("work", n.workSuggestions(ctx))
	}
	return err
}

func (n *Norn) readPersonalCalendar(ctx context.Context) error {
	if n.hidden(ctx, "google-calendar") {
		n.Accounts.Personal.Clear()
		return nil
	}
	if n.Accounts.Google.Status().State != "signed-in" || !n.Accounts.Google.Grants().Calendar {
		return nil
	}
	err := n.Accounts.Personal.readInto(func() (CalendarSnapshot, error) {
		return ReadGoogle(ctx, n.Accounts.Google, n.Accounts.get, time.Now(), n.logf)
	})
	if err == nil {
		n.calendarEvents("personal", n.Accounts.Personal)
	}
	return err
}

type microsoftAnswer struct {
	MicrosoftStatus
	Calendar CalendarRead `json:"calendar"`
}

func (n *Norn) microsoftStatus(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, microsoftAnswer{n.Accounts.Microsoft.Status(), n.workCalendarState(r.Context())})
}

var tenantName = regexp.MustCompile(`^[\w.-]{1,100}$`)

func (n *Norn) microsoftSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tenant   *string `json:"tenant"`
		Mail     bool    `json:"mail"`
		Calendar bool    `json:"calendar"`
		Force    bool    `json:"force"`
	}
	if err := part.Decode(r, &body); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant := "organizations"
	if body.Tenant != nil {
		if !tenantName.MatchString(*body.Tenant) {
			part.Error(w, http.StatusBadRequest, "`tenant` must be a tenant id or domain")
			return
		}
		tenant = *body.Tenant
	}
	status := n.Accounts.Microsoft.SignIn(r.Context(), tenant, MicrosoftScopes{Mail: body.Mail, Calendar: body.Calendar}, body.Force)
	part.JSON(w, http.StatusOK, microsoftAnswer{status, n.workCalendarState(r.Context())})
}

// microsoftToken hands out an access token for Graph, for the widget's own calls.
func (n *Norn) microsoftToken(w http.ResponseWriter, r *http.Request) {
	token, err := n.Accounts.Microsoft.Token(r.Context())
	if errors.Is(err, ErrSignedOut) {
		part.Error(w, http.StatusUnauthorized, "not signed in to Microsoft")
		return
	}
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, token)
}

func (n *Norn) microsoftSignOut(w http.ResponseWriter, r *http.Request) {
	status := n.Accounts.Microsoft.SignOut()
	n.Accounts.Work.Clear()
	part.JSON(w, http.StatusOK, microsoftAnswer{status, n.workCalendarState(r.Context())})
}

type googleAnswer struct {
	GoogleStatus
	Calendar CalendarRead `json:"calendar"`
}

func (n *Norn) googleStatus(w http.ResponseWriter, r *http.Request) {
	part.JSON(w, http.StatusOK, googleAnswer{n.Accounts.Google.Status(), n.personalCalendarState(r.Context())})
}

func (n *Norn) googleSignIn(w http.ResponseWriter, r *http.Request) {
	status, err := n.Accounts.Google.SignIn()
	if err != nil {
		part.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, googleAnswer{status, n.personalCalendarState(r.Context())})
}

func (n *Norn) googleSignOut(w http.ResponseWriter, r *http.Request) {
	status := n.Accounts.Google.SignOut()
	n.Accounts.Personal.Clear()
	part.JSON(w, http.StatusOK, googleAnswer{status, n.personalCalendarState(r.Context())})
}

func (n *Norn) calendarRoute(calendar *Calendar) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if snapshot := calendar.Current(time.Now()); snapshot != nil {
			part.JSON(w, http.StatusOK, snapshot)
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"events": []any{}, "from": nil, "to": nil, "at": nil})
	}
}
