package norn

// The person recap before a meeting (GET /v1/people/recap/next): ten minutes
// before a meeting with someone else in it, the recaps of its main people, for
// the overview. The widget asks for it, as it asks for the gap and the save
// point; no message is sent, so it never reaches ntfy, and nothing is offered
// while a break you kept free or a focus block holds you.

import (
	"context"
	"net/http"
	"slices"
	"sort"
	"time"

	"yggdrasil/internal/part"
)

const (
	// RecapLead is how long before a meeting its people's recap is offered.
	RecapLead = 10 * time.Minute
	// recapMainPeople is the most people in a meeting whose recaps are given;
	// past it, only the organizer's.
	recapMainPeople = 3
)

// RecapMeeting is the meeting a recap is offered before.
type RecapMeeting struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Start   int64  `json:"start"`
}

// RecapNext is the recaps for the main people of the meeting about to start.
type RecapNext struct {
	On      bool          `json:"on"`
	Meeting *RecapMeeting `json:"meeting,omitempty"`
	People  []PersonRecap `json:"people"`
}

// NextRecapMeeting is the meeting whose people's recap is offered at now: the
// first with someone else in it that starts within RecapLead, not declined,
// not all day, not shown as free, not a break or a focus block. None while a
// break you kept free or a focus block you booked holds you: the recap waits
// until it ends, as Hermóðr holds messages then.
func NextRecapMeeting(events []CalendarEvent, now time.Time) (CalendarEvent, bool) {
	nowMs := now.UnixMilli()
	var found CalendarEvent
	ok := false
	for _, e := range events {
		if (e.Break || e.Focus) && e.Start <= nowMs && e.End > nowMs {
			return CalendarEvent{}, false
		}
		if e.Break || e.Focus || declined(e) || e.AllDay || e.ShowAs == "free" || (len(e.Attendees) == 0 && e.Organizer == "") {
			continue
		}
		if e.Start <= nowMs || e.Start-nowMs > RecapLead.Milliseconds() {
			continue
		}
		if !ok || e.Start < found.Start {
			found, ok = e, true
		}
	}
	return found, ok
}

// MainPeople is whose recap a meeting gets: everyone when it is small, else
// only the organizer, and no one in a big meeting you organize.
func MainPeople(invited []invitee) []invitee {
	if len(invited) <= recapMainPeople {
		out := slices.Clone(invited)
		sort.SliceStable(out, func(a, b int) bool { return out[a].Organizer && !out[b].Organizer })
		return out
	}
	for _, i := range invited {
		if i.Organizer {
			return []invitee{i}
		}
	}
	return nil
}

// Next is the recaps for the meeting about to start, if one is.
func (r *Recaps) Next(ctx context.Context) (RecapNext, error) {
	next := RecapNext{On: true, People: []PersonRecap{}}
	now := r.now()
	snapshot := r.n.Accounts.Work.Current(now)
	if snapshot == nil {
		return next, nil
	}
	event, ok := NextRecapMeeting(snapshot.Events, now)
	if !ok {
		return next, nil
	}
	people := MainPeople(r.Invited(ctx, event))
	if len(people) == 0 {
		return next, nil
	}
	next.Meeting = &RecapMeeting{ID: event.ID, Subject: event.Subject, Start: event.Start}
	for _, i := range people {
		recap, err := r.For(ctx, RecapPerson{Name: i.Name, Address: i.Address})
		if err != nil {
			return next, err
		}
		next.People = append(next.People, recap)
	}
	return next, nil
}

// meetingInvitees is who is in a meeting: from its invitation through Graph,
// with addresses, you and rooms left out; else the calendar's names.
func (n *Norn) meetingInvitees(ctx context.Context, event CalendarEvent) []invitee {
	if event.ID != "" && n.Inbox != nil {
		if token, err := n.Accounts.Microsoft.Token(ctx); err == nil {
			get := func(ctx context.Context, path string, into any) error {
				return graphJSON(ctx, token.AccessToken, path, into)
			}
			if me, err := n.Inbox.Graph.Me(ctx, token.AccessToken); err == nil {
				if invitation, err := readInvitation(ctx, get, event.ID, me); err == nil {
					return invitation.Invitees
				}
			}
		}
	}
	var out []invitee
	if event.Organizer != "" {
		out = append(out, invitee{Name: event.Organizer, Organizer: true})
	}
	for _, name := range event.Attendees {
		if !slices.ContainsFunc(out, func(i invitee) bool { return SamePersonName(i.Name, name) }) {
			out = append(out, invitee{Name: name})
		}
	}
	return out
}

func (n *Norn) mountRecapNext(p *part.Part) {
	p.Handle("GET /v1/people/recap/next", n.nextRecap)
}

func (n *Norn) nextRecap(w http.ResponseWriter, r *http.Request) {
	next, err := n.Recaps.Next(r.Context())
	if err != nil {
		n.fail(w, err)
		return
	}
	part.JSON(w, http.StatusOK, next)
}
