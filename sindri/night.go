package sindri

// The night shift: drafts you mark "run tonight" start on their own in the
// evening, and in the morning one message through Hermóðr says what is ready
// to review and what got stuck. A night runs from Start until Report the next
// morning; a goal marked during it starts within a minute. Each goal runs one
// night: its mark is cleared when it starts.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// NightShift is when the night starts and when its report goes out, as HH:MM
// in Sindri's time zone.
type NightShift struct {
	Start  string `json:"start"`
	Report string `json:"report"`
}

// NightRun is one night: the goals it started and whether it has reported.
type NightRun struct {
	Night    string         `json:"night"` // the date the night started, 2006-01-02
	Goals    []int64        `json:"goals"`
	Failed   map[string]int `json:"failed,omitempty"` // repo: how many goals could not start it
	Reported bool           `json:"reported"`
}

var clockTime = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Night is the night shift's times; 22:00 to 07:00 until set.
func (w *Work) Night() NightShift {
	night := NightShift{Start: "22:00", Report: "07:00"}
	w.Store.Setting("night-shift", &night)
	return night
}

func (w *Work) zone() *time.Location {
	if w.Zone == nil {
		return time.Local
	}
	return w.Zone
}

func minutes(clock string) int {
	var h, m int
	_, _ = fmt.Sscanf(clock, "%d:%d", &h, &m)
	return h*60 + m
}

// nightOf is the night now belongs to (the date it started), or "" outside one.
func nightOf(now time.Time, shift NightShift) string {
	at, start, report := now.Hour()*60+now.Minute(), minutes(shift.Start), minutes(shift.Report)
	switch {
	case start == report:
		return ""
	case start < report: // a night that does not cross midnight
		if at >= start && at < report {
			return now.Format(time.DateOnly)
		}
	case at >= start:
		return now.Format(time.DateOnly)
	case at < report:
		return now.AddDate(0, 0, -1).Format(time.DateOnly)
	}
	return ""
}

// RunNight starts tonight's goals during the night, and reports once it is over.
func (w *Work) RunNight(ctx context.Context) error {
	now := w.Store.now().In(w.zone())
	var run NightRun
	w.Store.Setting("night-run", &run)
	night := nightOf(now, w.Night())
	if night == "" {
		if run.Night != "" && !run.Reported {
			return w.reportNight(ctx, &run)
		}
		return nil
	}
	fresh := run.Night != night
	if fresh {
		if run.Night != "" && !run.Reported {
			// The last night never reported (Sindri was down through the morning).
			if err := w.reportNight(ctx, &run); err != nil {
				return err
			}
		}
		run = NightRun{Night: night}
		w.selfRepairTonight(ctx, night)
	}
	goals, err := w.GoalViews()
	if err != nil {
		return err
	}
	started := false
	for _, goal := range goals {
		if !goal.Night {
			continue
		}
		_ = w.Store.SetGoalNight(goal.ID, false)
		if goal.State != "draft" || len(goal.Repos) == 0 {
			continue
		}
		_, failed, err := w.StartGoal(goal.ID, nil)
		if err != nil {
			w.logf("night shift: could not start goal %d: %v", goal.ID, err)
			continue
		}
		for _, repo := range failed {
			if run.Failed == nil {
				run.Failed = map[string]int{}
			}
			run.Failed[repo]++
		}
		run.Goals = append(run.Goals, goal.ID)
		started = true
		w.logf("night shift: started goal %d", goal.ID)
	}
	if started || fresh {
		return w.Store.SetSetting("night-run", run)
	}
	return nil
}

// NightReport is the morning message for the goals a night started.
func NightReport(goals []GoalView) tell.Message {
	groups := []struct {
		label  string
		states []string
	}{
		{"Ready to review", []string{"done"}},
		{"Needs you", []string{"needs-you"}},
		{"Still running", []string{"running", "draft"}},
		{"Merged or closed", []string{"merged", "closed"}},
	}
	var lines []string
	for _, group := range groups {
		var in []string
		for _, goal := range goals {
			if slices.Contains(group.states, goal.State) {
				line := "- " + goal.Title
				if goal.State == "needs-you" && goal.Next.Text != "" {
					line += ": " + goal.Next.Text
				}
				in = append(in, line)
			}
		}
		if len(in) > 0 {
			lines = append(lines, group.label+":\n"+strings.Join(in, "\n"))
		}
	}
	title := "Night shift is done"
	if slices.ContainsFunc(goals, func(g GoalView) bool { return g.State == "done" }) {
		title = "Night shift: work is ready to review"
	}
	if slices.ContainsFunc(goals, func(g GoalView) bool { return g.State == "needs-you" }) {
		title = "Night shift: something needs you"
	}
	return tell.Message{Title: title, Text: strings.Join(lines, "\n\n"), Priority: "normal", Hold: true}
}

func (w *Work) reportNight(ctx context.Context, run *NightRun) error {
	if len(run.Goals) > 0 && w.Hermodr != nil && w.Hermodr.On() {
		views, err := w.GoalViews()
		if err != nil {
			return err
		}
		var goals []GoalView
		for _, view := range views {
			if slices.Contains(run.Goals, view.ID) {
				goals = append(goals, view)
			}
		}
		if len(goals) > 0 {
			if err := w.Hermodr.Tell(ctx, NightReportWithRepairs(goals)); err != nil {
				return err
			}
		}
	}
	run.Reported = true
	return w.Store.SetSetting("night-run", run)
}

func (w *Work) mountNight(p *part.Part) {
	p.Handle("GET /v1/settings/night-shift", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, w.Night())
	})
	p.Handle("PATCH /v1/settings/night-shift", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Start  *string `json:"start"`
			Report *string `json:"report"`
		}
		bad := func(s *string) bool { return s != nil && !clockTime.MatchString(*s) }
		if err := part.Decode(r, &body); err != nil || (body.Start == nil && body.Report == nil) || bad(body.Start) || bad(body.Report) {
			part.Error(rw, http.StatusBadRequest, "send `start` and/or `report` as HH:MM")
			return
		}
		night := w.Night()
		if body.Start != nil {
			night.Start = *body.Start
		}
		if body.Report != nil {
			night.Report = *body.Report
		}
		if night.Start == night.Report {
			part.Error(rw, http.StatusBadRequest, "`start` and `report` must differ")
			return
		}
		if err := w.Store.SetSetting("night-shift", night); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, night)
	})
}
