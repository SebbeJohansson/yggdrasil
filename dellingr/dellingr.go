// Package dellingr runs the jobs you put on a clock: "every weekday 08:00,
// summarise my calendar and unread mail", "1st of each month, draft a goal
// to update dependencies in glim". Reacting to events belongs to Huginn, and
// each part's own fixed loops stay in that part.
//
//	POST   /v1/parse          words to cron, with the next three runs; saves nothing
//	GET    /v1/jobs           every job, by next run
//	POST   /v1/jobs           a new job; disabled unless the owner made it
//	GET    /v1/jobs/{id}      the job and its last 20 runs
//	PATCH  /v1/jobs/{id}      change it; enabling and autoStart are owner only
//	DELETE /v1/jobs/{id}
//	POST   /v1/jobs/{id}/run  run it now, once; owner only
//	GET    /v1/runs           every job's runs lately
package dellingr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
	"yggdrasil/internal/tell"
)

// Name is this part's name.
const Name = "dellingr"

const (
	// Tick is how often the clock looks for jobs that are due.
	Tick = 30 * time.Second
	// Late is when a due run counts as missed: the machine slept through it.
	Late = 2 * time.Minute
	// KeepRuns is how long runs are listed.
	KeepRuns = 30 * 24 * time.Hour
	// FailingAfter is how many failed runs in a row are raised on Gjallar.
	FailingAfter = 3
	// AskSession is the Norn conversation Dellingr's questions go into.
	AskSession = "dellingr"
)

// Action is what a job does: notify, ask or goal.
type Action struct {
	Kind      string   `json:"kind"`
	Text      string   `json:"text,omitempty"`
	Title     string   `json:"title,omitempty"`
	Repos     []string `json:"repos,omitempty"`
	AutoStart bool     `json:"autoStart,omitempty"`
}

// Job is one thing on the clock.
type Job struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Cron         string     `json:"cron"`
	Timezone     string     `json:"timezone"`
	Action       Action     `json:"action"`
	Deliver      []string   `json:"deliver"`
	Enabled      bool       `json:"enabled"`
	CreatedBy    string     `json:"createdBy"`
	Said         string     `json:"said"`
	NextRunAt    *time.Time `json:"nextRunAt,omitempty"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	LastResult   string     `json:"lastResult,omitempty"`
	FailingSince *time.Time `json:"failingSince,omitempty"`
	Failures     int        `json:"-"`
}

// Run is one time a job ran.
type Run struct {
	ID        string     `json:"id"`
	JobID     string     `json:"jobId"`
	JobName   string     `json:"jobName"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	State     string     `json:"state"`
	Missed    bool       `json:"missed"`
	ByHand    bool       `json:"byHand,omitempty"`
	Text      string     `json:"text,omitempty"`
	GoalID    string     `json:"goalId,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// stored is a job with what the API doesn't show.
type stored struct {
	Job
	Failures int `json:"failures"`
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, job_id TEXT NOT NULL, at INTEGER NOT NULL, body TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS runs_at ON runs (at);
`

// Store keeps the jobs and the last 30 days of runs.
type Store struct {
	db *sql.DB
}

// ErrNotFound is a job that isn't there.
var ErrNotFound = errors.New("no such job")

// OpenStore opens (or makes) the SQLite file.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("dellingr schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) jobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		var body string
		var job stored
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &job) == nil {
			job.Job.Failures = job.Failures
			jobs = append(jobs, job.Job)
		}
	}
	return jobs, rows.Err()
}

func (s *Store) job(ctx context.Context, id string) (Job, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM jobs WHERE id = ?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	var job stored
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		return Job{}, err
	}
	job.Job.Failures = job.Failures
	return job.Job, nil
}

func (s *Store) putJob(ctx context.Context, job Job) error {
	body, err := json.Marshal(stored{Job: job, Failures: job.Failures})
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs (id, body) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET body = excluded.body`, job.ID, string(body))
	return err
}

func (s *Store) deleteJob(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) putRun(ctx context.Context, run Run) error {
	body, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runs (id, job_id, at, body) VALUES (?, ?, ?, ?) ON CONFLICT (id) DO UPDATE SET body = excluded.body`,
		run.ID, run.JobID, run.StartedAt.UnixMilli(), string(body))
	return err
}

func (s *Store) runs(ctx context.Context, jobID string, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM runs WHERE ? = '' OR job_id = ? ORDER BY at DESC LIMIT ?`, jobID, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var body string
		var run Run
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &run) == nil {
			runs = append(runs, run)
		}
	}
	return runs, rows.Err()
}

func (s *Store) forget(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE at < ?`, before.UnixMilli())
	return err
}

// Dellingr is the running service.
type Dellingr struct {
	Store   *Store
	Norn    *call.Part
	Sindri  *call.Part
	Gjallar *alert.Client
	// Tell carries each run's result to you (Hermóðr).
	Tell    tell.Teller
	// Zone is this machine's, for jobs that name none.
	Zone *time.Location

	now     func() time.Time
	logf    func(string, ...any)
	mu      sync.Mutex
	running map[string]bool
}

// New makes Dellingr.
func New(store *Store, norn, sindri *call.Part, gjallar *alert.Client, teller tell.Teller, zone *time.Location, logf func(string, ...any)) *Dellingr {
	return &Dellingr{Store: store, Norn: norn, Sindri: sindri, Gjallar: gjallar, Tell: teller, Zone: zone, now: time.Now, logf: logf, running: map[string]bool{}}
}

// Mount adds the routes and the clock.
func (d *Dellingr) Mount(p *part.Part) {
	p.Handle("POST /v1/parse", d.parse)
	p.Handle("GET /v1/jobs", d.listJobs)
	p.Handle("POST /v1/jobs", d.createJob)
	p.Handle("GET /v1/jobs/{id}", d.getJob)
	p.Handle("PATCH /v1/jobs/{id}", d.changeJob)
	p.Handle("DELETE /v1/jobs/{id}", d.deleteJob)
	p.Handle("POST /v1/jobs/{id}/run", d.runNow)
	p.Handle("GET /v1/runs", d.listRuns)
	p.Loop("clock", Tick, d.Tick)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *Dellingr) zone(name string) (*time.Location, string, error) {
	if name == "" {
		return d.Zone, d.Zone.String(), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", fmt.Errorf("unknown time zone %q", name)
	}
	return loc, name, nil
}

// --- routes ---

func (d *Dellingr) parse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string `json:"text"`
		Timezone string `json:"timezone"`
	}
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	loc, zone, err := d.zone(in.Timezone)
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	line, err := Words(in.Text)
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	schedule, _ := ParseCron(line)
	next := []time.Time{}
	at := d.now()
	for range 3 {
		if at = schedule.Next(at, loc); at.IsZero() {
			break
		}
		next = append(next, at)
	}
	part.JSON(w, http.StatusOK, map[string]any{"cron": line, "timezone": zone, "said": Said(line), "next": next})
}

func (d *Dellingr) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := d.Store.jobs(r.Context())
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		a, b := jobs[i].NextRunAt, jobs[j].NextRunAt
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.Before(*b)
	})
	part.JSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

type jobInput struct {
	Name     *string   `json:"name"`
	Cron     *string   `json:"cron"`
	Timezone *string   `json:"timezone"`
	Action   *Action   `json:"action"`
	Deliver  *[]string `json:"deliver"`
	Enabled  *bool     `json:"enabled"`
}

// prepare checks the job and works out its schedule's words and next run.
func (d *Dellingr) prepare(job *Job) error {
	job.Name = strings.TrimSpace(job.Name)
	job.Cron = strings.Join(strings.Fields(job.Cron), " ")
	switch {
	case job.Name == "" || len([]rune(job.Name)) > 100:
		return errors.New("name must be 1 to 100 characters")
	case len(job.Action.Text) > 4000:
		return errors.New("action.text must be at most 4000 characters")
	}
	switch job.Action.Kind {
	case "notify", "ask":
		if strings.TrimSpace(job.Action.Text) == "" {
			return fmt.Errorf("a %s action needs text", job.Action.Kind)
		}
	case "goal":
		if t := strings.TrimSpace(job.Action.Title); t == "" || len([]rune(t)) > 200 {
			return errors.New("a goal action needs a title of 1 to 200 characters")
		}
		if len(job.Action.Repos) > 4 {
			return errors.New("a goal takes at most 4 repos")
		}
	default:
		return errors.New("action.kind must be notify, ask or goal")
	}
	for _, where := range job.Deliver {
		if where != "phone" && where != "widget" {
			return errors.New("deliver takes phone and widget")
		}
	}
	schedule, err := ParseCron(job.Cron)
	if err != nil {
		return err
	}
	loc, zone, err := d.zone(job.Timezone)
	if err != nil {
		return err
	}
	job.Timezone, job.Said = zone, Said(job.Cron)
	next := schedule.Next(d.now(), loc)
	if next.IsZero() {
		return errors.New("that schedule never runs")
	}
	job.NextRunAt = &next
	return nil
}

func (in jobInput) apply(job *Job) {
	if in.Name != nil {
		job.Name = *in.Name
	}
	if in.Cron != nil {
		job.Cron = *in.Cron
	}
	if in.Timezone != nil {
		job.Timezone = *in.Timezone
	}
	if in.Action != nil {
		job.Action = *in.Action
	}
	if in.Deliver != nil {
		job.Deliver = *in.Deliver
	}
}

func (d *Dellingr) createJob(w http.ResponseWriter, r *http.Request) {
	var in jobInput
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	caller := part.Caller(r)
	if in.Action != nil && in.Action.AutoStart && caller != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may set autoStart")
		return
	}
	job := Job{ID: newID(), Deliver: []string{"phone", "widget"}, CreatedBy: caller, Enabled: caller == part.Owner}
	in.apply(&job)
	if err := d.prepare(&job); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.Store.putJob(r.Context(), job); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusCreated, job)
}

func (d *Dellingr) found(w http.ResponseWriter, r *http.Request) (Job, bool) {
	job, err := d.Store.job(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		part.Error(w, http.StatusNotFound, "no such job")
		return job, false
	case err != nil:
		part.Error(w, http.StatusInternalServerError, err.Error())
		return job, false
	}
	return job, true
}

func (d *Dellingr) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := d.found(w, r)
	if !ok {
		return
	}
	runs, err := d.Store.runs(r.Context(), job.ID, 20)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"job": job, "runs": runs})
}

func (d *Dellingr) changeJob(w http.ResponseWriter, r *http.Request) {
	job, ok := d.found(w, r)
	if !ok {
		return
	}
	var in jobInput
	if err := part.Decode(r, &in); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	caller := part.Caller(r)
	switch {
	case caller != part.Owner && caller != job.CreatedBy:
		part.Error(w, http.StatusForbidden, "only the owner may change a job another made")
		return
	case caller != part.Owner && in.Enabled != nil && *in.Enabled:
		part.Error(w, http.StatusForbidden, "only the owner may enable a job")
		return
	case caller != part.Owner && in.Action != nil && in.Action.AutoStart:
		part.Error(w, http.StatusForbidden, "only the owner may set autoStart")
		return
	}
	in.apply(&job)
	if in.Enabled != nil {
		job.Enabled = *in.Enabled
	}
	if err := d.prepare(&job); err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.Store.putJob(r.Context(), job); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, job)
}

func (d *Dellingr) deleteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := d.found(w, r)
	if !ok {
		return
	}
	if caller := part.Caller(r); caller != part.Owner && caller != job.CreatedBy {
		part.Error(w, http.StatusForbidden, "only the owner may remove a job another made")
		return
	}
	if err := d.Store.deleteJob(r.Context(), job.ID); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Dellingr) runNow(w http.ResponseWriter, r *http.Request) {
	if part.Caller(r) != part.Owner {
		part.Error(w, http.StatusForbidden, "only the owner may run a job by hand")
		return
	}
	job, ok := d.found(w, r)
	if !ok {
		return
	}
	run := Run{ID: newID(), JobID: job.ID, JobName: job.Name, StartedAt: d.now(), State: "running", ByHand: true}
	if err := d.Store.putRun(r.Context(), run); err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	go d.finish(context.WithoutCancel(r.Context()), job.ID, run)
	part.JSON(w, http.StatusAccepted, run)
}

func (d *Dellingr) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := d.Store.runs(r.Context(), "", 200)
	if err != nil {
		part.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// --- the clock ---

// Tick runs every enabled job that is due. A run that is more than two
// minutes late was slept through: it runs once, marked missed, and the next
// run is counted from now, so a night asleep is never caught up tick by tick.
func (d *Dellingr) Tick(ctx context.Context) error {
	if err := d.Store.forget(ctx, d.now().Add(-KeepRuns)); err != nil {
		return err
	}
	jobs, err := d.Store.jobs(ctx)
	if err != nil {
		return err
	}
	now := d.now()
	var wg sync.WaitGroup
	for _, job := range jobs {
		if !job.Enabled || job.NextRunAt == nil || job.NextRunAt.After(now) {
			continue
		}
		missed := now.Sub(*job.NextRunAt) > Late
		if err := d.prepare(&job); err != nil {
			d.logf("job %s: %v", job.ID, err)
			continue
		}
		if err := d.Store.putJob(ctx, job); err != nil {
			return err
		}
		run := Run{ID: newID(), JobID: job.ID, JobName: job.Name, StartedAt: now, State: "running", Missed: missed}
		if err := d.Store.putRun(ctx, run); err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.finish(ctx, job.ID, run)
		}()
	}
	wg.Wait()
	return nil
}

// finish does the job's work for the run and records how it went. A job
// already running is skipped, not run twice at once.
func (d *Dellingr) finish(ctx context.Context, jobID string, run Run) {
	d.mu.Lock()
	busy := d.running[jobID]
	d.running[jobID] = true
	d.mu.Unlock()
	defer func() {
		if !busy {
			d.mu.Lock()
			delete(d.running, jobID)
			d.mu.Unlock()
		}
	}()
	job, err := d.Store.job(ctx, jobID)
	switch {
	case err != nil:
		run.State, run.Error = "failed", err.Error()
	case busy:
		run.State, run.Error = "skipped", "the last run is still going"
	default:
		run.Text, run.GoalID, err = d.work(ctx, job)
		run.State = "done"
		if err != nil {
			run.State, run.Error = "failed", err.Error()
		}
	}
	ended := d.now()
	run.EndedAt = &ended
	if err := d.Store.putRun(ctx, run); err != nil {
		d.logf("keeping a run: %v", err)
	}
	if job.ID == "" || run.State == "skipped" {
		return
	}
	// Read it again: it may have changed while the run went.
	if current, err := d.Store.job(ctx, jobID); err == nil {
		job = current
	}
	job.LastRunAt, job.LastResult = &run.StartedAt, run.State
	key := "failing:" + job.ID
	if run.State == "failed" {
		job.Failures++
		if job.FailingSince == nil {
			job.FailingSince = &run.StartedAt
		}
		if job.Failures >= FailingAfter {
			_ = d.Gjallar.Raise(ctx, alert.Raise{Key: key, Severity: alert.Warning,
				Title: fmt.Sprintf("The job %q failed %d times in a row", job.Name, job.Failures), Detail: run.Error})
		}
	} else {
		if job.Failures >= FailingAfter {
			_ = d.Gjallar.Resolve(ctx, key)
		}
		job.Failures, job.FailingSince = 0, nil
	}
	if err := d.Store.putJob(ctx, job); err != nil {
		d.logf("keeping job %s: %v", job.ID, err)
	}
}

// work does the job's action and delivers what it says.
func (d *Dellingr) work(ctx context.Context, job Job) (text, goalID string, err error) {
	switch job.Action.Kind {
	case "notify":
		text = job.Action.Text
	case "ask":
		if text, err = d.Norn.Ask(ctx, job.Action.Text, AskSession); err != nil {
			return "", "", err
		}
	case "goal":
		var goal struct {
			ID string `json:"id"`
		}
		body := map[string]any{"title": job.Action.Title, "repos": job.Action.Repos, "context": job.Action.Text, "source": "dellingr:" + job.ID}
		if err := d.Sindri.Do(ctx, http.MethodPost, "/v1/goals", body, &goal); err != nil {
			return "", "", err
		}
		goalID, text = goal.ID, "Drafted the goal "+job.Action.Title+"."
		if job.Action.AutoStart {
			if err := d.Sindri.Do(ctx, http.MethodPost, "/v1/goals/"+goal.ID+"/start", map[string]any{}, nil); err != nil {
				return text, goalID, fmt.Errorf("drafted the goal, but could not start it: %w", err)
			}
			text = "Started the goal " + job.Action.Title + "."
		}
	default:
		return "", "", fmt.Errorf("unknown action %q", job.Action.Kind)
	}
	if d.Tell != nil && d.Tell.On() {
		push := slices.Contains(job.Deliver, "phone")
		if err := d.Tell.Tell(ctx, tell.Message{Title: job.Name, Text: text, Priority: "normal", Hold: true, Push: &push}); err != nil {
			return text, goalID, fmt.Errorf("telling you: %w", err)
		}
	}
	return text, goalID, nil
}
