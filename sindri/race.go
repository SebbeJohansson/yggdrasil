package sindri

// Races: two coding agents on the same goal. You mark a draft goal "race"
// beside "run tonight"; the night shift then starts it twice, once on each
// conduit of the pair (Claude Code and OpenCode until set), each in its own
// worktree and branch. The morning report says which races are ready, and
// you pick the winner: its branch merges through the usual merge, the
// loser's worktree and branch are removed. "Neither" removes both and closes
// the goal. Each pick is kept with the goal's kind (ui, bugfix, tests…), and
// once a kind has enough picks, a goal of that kind that is not raced shows
// the conduit that wins it as a suggestion; you still choose.
//
// A kind is judged from the goal's text by a KindJudge: Norn's local model
// when Sindri can reach Norn, else a keyword guess, which is also what a goal
// shows until the model has answered.

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/part"
)

const raceSchema = `
CREATE TABLE IF NOT EXISTS goal_races (
	goal_id INTEGER PRIMARY KEY,
	race INTEGER NOT NULL DEFAULT 0,
	conduit TEXT NOT NULL DEFAULT '',
	started_at INTEGER,
	pair TEXT NOT NULL DEFAULT '[]',
	note TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS race_lanes (
	task_id INTEGER PRIMARY KEY,
	goal_id INTEGER NOT NULL,
	conduit TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS race_lanes_by_goal ON race_lanes (goal_id);
CREATE TABLE IF NOT EXISTS race_picks (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	goal_id INTEGER NOT NULL UNIQUE,
	kind TEXT NOT NULL,
	pair TEXT NOT NULL,
	winner TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS goal_kinds (
	goal_id INTEGER PRIMARY KEY,
	text TEXT NOT NULL,
	kind TEXT NOT NULL,
	by TEXT NOT NULL
);
`

// --- conduits ------------------------------------------------------------------------

// Conduit is a way to run a coding agent that a race can use.
type Conduit struct {
	Name, Label string
	// Engine is the agent engine it starts with.
	Engine string
	// Up is whether it can start an agent now.
	Up func(m *Manager) bool
	// Off is whether it is turned off on this machine (OPENCODE_URL=off): then
	// it is not listed, raced, suggested or chosen at all. nil is never off.
	Off func(m *Manager) bool
}

func binUp(bin string) bool {
	if info, err := os.Stat(bin); err == nil && !info.IsDir() {
		return true
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// Conduits are the coding agents a race picks from. Another one is one more
// entry, with an engine the manager starts.
var Conduits = []Conduit{
	{Name: "claude", Label: "Claude Code", Engine: "claude", Up: func(m *Manager) bool { return binUp(m.options.ClaudeBin) }},
	{Name: "opencode", Label: "OpenCode", Engine: "opencode", Up: func(m *Manager) bool { return m.OpencodeOn() }, Off: func(m *Manager) bool { return m == nil || !m.OpencodeSetUp() }},
	{Name: "copilot", Label: "Copilot CLI", Engine: "copilot", Up: func(m *Manager) bool { return binUp(m.options.CopilotBin) }},
}

func conduitNamed(name string) *Conduit {
	for i := range Conduits {
		if Conduits[i].Name == name {
			return &Conduits[i]
		}
	}
	return nil
}

// conduit is the conduit with that name, unless it is unknown or turned off here.
func (w *Work) conduit(name string) *Conduit {
	if c := conduitNamed(name); c != nil && (c.Off == nil || !c.Off(w.Agents)) {
		return c
	}
	return nil
}

// conduits is every conduit that is not turned off here.
func (w *Work) conduits() []Conduit {
	var on []Conduit
	for _, c := range Conduits {
		if w.conduit(c.Name) != nil {
			on = append(on, c)
		}
	}
	return on
}

func conduitLabel(name string) string {
	if c := conduitNamed(name); c != nil {
		return c.Label
	}
	return name
}

// ConduitInfo is a conduit as the settings list it.
type ConduitInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Up    bool   `json:"up"`
}

// RaceSettings are which two conduits race, and how many picks a kind needs
// before a winner is suggested for it.
type RaceSettings struct {
	Pair     []string `json:"pair"`
	MinPicks int      `json:"minPicks"`
}

// Races is the race settings; Claude Code against OpenCode and 5 picks until
// set. A pair with a conduit turned off here is the first two that are on.
func (w *Work) Races() RaceSettings {
	settings := RaceSettings{Pair: []string{"claude", "opencode"}, MinPicks: 5}
	w.Store.Setting("races", &settings)
	if len(settings.Pair) != 2 || w.conduit(settings.Pair[0]) == nil || w.conduit(settings.Pair[1]) == nil {
		on := w.conduits()
		settings.Pair = []string{}
		for _, c := range on[:min(2, len(on))] {
			settings.Pair = append(settings.Pair, c.Name)
		}
	}
	return settings
}

// --- kinds ---------------------------------------------------------------------------

// Kinds are what a goal can be judged to be.
var Kinds = []string{"ui", "bugfix", "refactor", "tests", "docs", "backend", "other"}

// KindJudge judges a goal's kind from its text.
type KindJudge interface {
	Judge(ctx context.Context, title, context string) (string, error)
}

var kindWords = []struct {
	kind  string
	words *regexp.Regexp
}{
	{"bugfix", regexp.MustCompile(`(?i)\b(fix|bug|broken|crash|error|fails?|failing|wrong|regression|doesn'?t work|not working)\b`)},
	{"tests", regexp.MustCompile(`(?i)\b(tests?|testing|coverage|spec|e2e|unit test)\b`)},
	{"docs", regexp.MustCompile(`(?i)\b(docs?|documentation|readme|guide|changelog|comment)\b`)},
	{"refactor", regexp.MustCompile(`(?i)\b(refactor|clean ?up|simplify|rename|restructure|split|extract|tidy)\b`)},
	{"ui", regexp.MustCompile(`(?i)\b(ui|page|button|screen|layout|css|style|widget|dashboard|view|modal|toggle|icon|design|dark mode|component)\b`)},
	{"backend", regexp.MustCompile(`(?i)\b(api|route|endpoint|database|db|server|sql|migration|queue|loop|service|auth)\b`)},
}

// KeywordKind is the kind a goal's words point to, the first that matches
// in the order above; "other" when none does.
func KeywordKind(title, context string) string {
	for _, text := range []string{title, context} {
		for _, k := range kindWords {
			if k.words.MatchString(text) {
				return k.kind
			}
		}
	}
	return "other"
}

// KindAsk is what Norn's local model is asked about a goal.
const KindAsk = `What kind of coding work is this goal? Answer with one word from: ui, bugfix, refactor, tests, docs, backend, other.

Goal: %s
%s`

// NornKinds asks Norn's local model (`/local`), so the goal never leaves this machine.
type NornKinds struct{ Norn *call.Part }

// Judge asks Norn and reads the first known kind in its answer.
func (n NornKinds) Judge(ctx context.Context, title, context string) (string, error) {
	if !n.Norn.On() {
		return "", call.ErrOff
	}
	answer, err := n.Norn.Ask(ctx, fmt.Sprintf(KindAsk, title, Clip(context, 1500)), "")
	if err != nil {
		return "", err
	}
	if kind := ReadKind(answer); kind != "" {
		return kind, nil
	}
	return "", fmt.Errorf("no kind in %q", Clip(answer, 80))
}

// ReadKind is the first known kind in an answer, or "".
func ReadKind(answer string) string {
	for _, word := range strings.FieldsFunc(strings.ToLower(answer), func(r rune) bool { return !('a' <= r && r <= 'z') }) {
		if slices.Contains(Kinds, word) {
			return word
		}
	}
	return ""
}

var (
	kindMu    sync.Mutex
	kindTried = map[string]time.Time{}
)

// kindRetry is how long a model judgment that failed waits before it is asked again.
const kindRetry = 30 * time.Minute

func goalText(goal Goal) string {
	sum := sha1.Sum([]byte(goal.Title + "\n" + goal.Context))
	return hex.EncodeToString(sum[:8])
}

// GoalKind is the goal's kind: the model's when it has judged this text,
// else the keyword guess while the model is asked in the background.
func (w *Work) GoalKind(goal Goal) string {
	text := goalText(goal)
	var stored, kind, by string
	_ = w.Store.db.QueryRow(`SELECT text, kind, by FROM goal_kinds WHERE goal_id = ?`, goal.ID).Scan(&stored, &kind, &by)
	if stored != text {
		kind, by = KeywordKind(goal.Title, goal.Context), "keywords"
		_, _ = w.Store.db.Exec(`INSERT INTO goal_kinds (goal_id, text, kind, by) VALUES (?, ?, ?, ?)
			ON CONFLICT (goal_id) DO UPDATE SET text = excluded.text, kind = excluded.kind, by = excluded.by`, goal.ID, text, kind, by)
	}
	if by == "model" || w.Kinds == nil {
		return kind
	}
	key := fmt.Sprintf("%p:%d:%s", w.Store, goal.ID, text)
	kindMu.Lock()
	tried, asked := kindTried[key]
	if !asked || time.Since(tried) > kindRetry {
		kindTried[key] = time.Now()
		go w.judgeKind(goal, text)
	}
	kindMu.Unlock()
	return kind
}

func (w *Work) judgeKind(goal Goal, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	kind, err := w.Kinds.Judge(ctx, goal.Title, goal.Context)
	if err != nil || !slices.Contains(Kinds, kind) {
		if err != nil && !errors.Is(err, call.ErrOff) {
			w.logf("could not judge goal %d's kind: %v", goal.ID, err)
		}
		return
	}
	_, _ = w.Store.db.Exec(`UPDATE goal_kinds SET kind = ?, by = 'model' WHERE goal_id = ? AND text = ?`, kind, goal.ID, text)
}

// --- the store -------------------------------------------------------------------------

type raceRow struct {
	race      bool
	conduit   string
	startedAt *int64
	pair      []string
	note      string
}

func (s *Store) raceOf(goal int64) raceRow {
	var row raceRow
	var pair string
	err := s.db.QueryRow(`SELECT race, conduit, started_at, pair, note FROM goal_races WHERE goal_id = ?`, goal).Scan(&row.race, &row.conduit, &row.startedAt, &pair, &row.note)
	if err == nil {
		_ = json.Unmarshal([]byte(pair), &row.pair)
	}
	return row
}

func (s *Store) ensureRace(goal int64) error {
	_, err := s.db.Exec(`INSERT INTO goal_races (goal_id) VALUES (?) ON CONFLICT (goal_id) DO NOTHING`, goal)
	return err
}

// SetGoalRace marks a goal to race at the night shift, or unmarks it.
func (s *Store) SetGoalRace(goal int64, race bool) error {
	if err := s.ensureRace(goal); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE goal_races SET race = ? WHERE goal_id = ?`, race, goal)
	return err
}

// SetGoalConduit is the conduit you chose for a goal's normal run; "" is the default.
func (s *Store) SetGoalConduit(goal int64, conduit string) error {
	if err := s.ensureRace(goal); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE goal_races SET conduit = ? WHERE goal_id = ?`, conduit, goal)
	return err
}

func (s *Store) raceStarted(goal int64, pair []string, note string) error {
	if err := s.ensureRace(goal); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE goal_races SET started_at = ?, pair = ?, note = ? WHERE goal_id = ?`, s.ms(), mustJSON(pair), note, goal)
	return err
}

func (s *Store) lanesOf(goal int64) map[int64]string {
	lanes := map[int64]string{}
	rows, err := s.db.Query(`SELECT task_id, conduit FROM race_lanes WHERE goal_id = ?`, goal)
	if err != nil {
		return lanes
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var conduit string
		if rows.Scan(&id, &conduit) == nil {
			lanes[id] = conduit
		}
	}
	return lanes
}

// RacePick is the winner you picked for a race; "neither" when none won.
type RacePick struct {
	At     int64    `json:"at"`
	Goal   int64    `json:"goal"`
	Kind   string   `json:"kind"`
	Pair   []string `json:"pair"`
	Winner string   `json:"winner"`
}

func (s *Store) picks(where string, args ...any) ([]RacePick, error) {
	rows, err := s.db.Query(`SELECT at, goal_id, kind, pair, winner FROM race_picks `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	picks := []RacePick{}
	for rows.Next() {
		var p RacePick
		var pair string
		if err := rows.Scan(&p.At, &p.Goal, &p.Kind, &pair, &p.Winner); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(pair), &p.Pair)
		picks = append(picks, p)
	}
	return picks, rows.Err()
}

func (s *Store) pickOf(goal int64) *RacePick {
	picks, _ := s.picks(`WHERE goal_id = ?`, goal)
	if len(picks) == 0 {
		return nil
	}
	return &picks[0]
}

// --- views ------------------------------------------------------------------------------

// RaceLane is one conduit's run of a race goal.
type RaceLane struct {
	Conduit string     `json:"conduit"`
	Label   string     `json:"label"`
	State   string     `json:"state"`
	Tasks   []GoalTask `json:"tasks"`
}

// RaceView is a started race: its lanes, whether both are ready to compare,
// and your pick once made.
type RaceView struct {
	Pair      []string   `json:"pair"`
	StartedAt int64      `json:"startedAt"`
	Lanes     []RaceLane `json:"lanes"`
	Ready     bool       `json:"ready"`
	Pick      *RacePick  `json:"pick"`
}

// ConduitSuggestion is the conduit that wins a kind most, from your picks.
type ConduitSuggestion struct {
	Conduit string `json:"conduit"`
	Kind    string `json:"kind"`
	Wins    int    `json:"wins"`
	Picks   int    `json:"picks"`
}

// GoalRaceFields are a goal's race fields, as the goal shows them.
type GoalRaceFields struct {
	// Race is whether the goal races at the night shift.
	Race bool `json:"race"`
	// Kind is the goal's kind, judged from its text.
	Kind string `json:"kind"`
	// Conduit is what you chose for a normal run; "" is Sindri's default.
	Conduit string `json:"conduit"`
	// Suggestion is the conduit that wins this kind, for a draft that does not race.
	Suggestion *ConduitSuggestion `json:"suggestion,omitempty"`
	// Racing is the race once started.
	Racing *RaceView `json:"racing,omitempty"`
	// RaceNote says why a goal marked to race ran once instead.
	RaceNote string `json:"raceNote,omitempty"`
}

func laneReady(tasks []GoalTask) bool {
	return len(tasks) > 0 && !slices.ContainsFunc(tasks, func(t GoalTask) bool { return t.Status == "running" || t.Status == "scheduled" })
}

// raceFields fills in a goal view's race fields; a started race's state and
// next step are read from its lanes.
func (w *Work) raceFields(view *GoalView) {
	row := w.Store.raceOf(view.ID)
	view.Race, view.Conduit, view.RaceNote = row.race, row.conduit, row.note
	view.Kind = w.GoalKind(view.Goal)
	if view.State == "draft" && !row.race && view.ClosedAt == nil {
		view.Suggestion = w.Suggest(view.Kind)
	}
	lanes := w.Store.lanesOf(view.ID)
	if row.startedAt == nil || len(lanes) == 0 {
		return
	}
	racing := &RaceView{Pair: row.pair, StartedAt: *row.startedAt, Lanes: []RaceLane{}, Pick: w.Store.pickOf(view.ID)}
	var all []GoalTask
	racing.Ready = true
	for _, conduit := range row.pair {
		lane := RaceLane{Conduit: conduit, Label: conduitLabel(conduit), Tasks: []GoalTask{}}
		for _, t := range view.Tasks {
			if lanes[t.ID] == conduit {
				lane.Tasks = append(lane.Tasks, t)
			}
		}
		current := currentParts(lane.Tasks)
		lane.State = GoalState(current)
		racing.Ready = racing.Ready && laneReady(current)
		all = append(all, current...)
		racing.Lanes = append(racing.Lanes, lane)
	}
	view.Racing = racing
	if view.ClosedAt != nil {
		return
	}
	switch pick := racing.Pick; {
	case pick != nil && pick.Winner != "neither":
		for _, lane := range racing.Lanes {
			if lane.Conduit == pick.Winner {
				current := currentParts(lane.Tasks)
				view.State, view.Next = GoalState(current), NextFor(current)
			}
		}
	case racing.Ready:
		view.State = "done"
		view.Next = GoalNext{Action: "pick", Text: "Pick the winner of the race"}
	default:
		view.State, view.Next = GoalState(all), NextFor(all)
		if view.State == "done" || view.State == "merged" {
			view.State, view.Next = "running", GoalNext{Action: "none", Text: "Agents are racing"}
		}
	}
}

// raceLines are the morning report's lines on races: which are ready to
// pick, still running, or ran once instead. The rest of the goals are
// returned for the usual groups.
func raceLines(goals []GoalView) (lines []string, ready bool, rest []GoalView) {
	var picks, running, once []string
	for _, goal := range goals {
		switch {
		case goal.Racing != nil && goal.Racing.Pick == nil && goal.ClosedAt == nil:
			names := make([]string, len(goal.Racing.Pair))
			for i, c := range goal.Racing.Pair {
				names[i] = conduitLabel(c)
			}
			line := "- " + goal.Title + " (" + strings.Join(names, " vs ") + ")"
			if goal.Racing.Ready {
				picks = append(picks, line)
			} else {
				running = append(running, line)
			}
		case goal.RaceNote != "" && goal.Racing == nil:
			once = append(once, "- "+goal.Title+": "+goal.RaceNote)
			rest = append(rest, goal)
		default:
			rest = append(rest, goal)
		}
	}
	if len(picks) > 0 {
		lines = append(lines, "Races ready to pick:\n"+strings.Join(picks, "\n"))
	}
	if len(running) > 0 {
		lines = append(lines, "Races still running:\n"+strings.Join(running, "\n"))
	}
	if len(once) > 0 {
		lines = append(lines, "Ran once, not raced:\n"+strings.Join(once, "\n"))
	}
	return lines, len(picks) > 0, rest
}

// --- starting --------------------------------------------------------------------------

// laneRequest dresses a goal's code request for a race lane, or, outside a
// race, for the conduit you chose for the goal.
func (w *Work) laneRequest(goal *Goal, lane string, request *CodeRequest) {
	if lane == "" {
		lane = w.Store.raceOf(goal.ID).conduit
		if c := w.conduit(lane); c != nil && c.Up(w.Agents) {
			request.Engine = c.Engine
		}
		return
	}
	c := conduitNamed(lane)
	request.Engine = c.Engine
	request.Name = ShortTask(goal.Title, 40) + " " + c.Name
	request.Goal = goal.Title + " (" + c.Label + ")"
}

// startTonight starts a goal the night shift took: as a race when marked,
// else as usual. It returns what could not start.
func (w *Work) startTonight(id int64) ([]string, error) {
	if w.Store.raceOf(id).race {
		_, failed, _, err := w.StartRace(id)
		return failed, err
	}
	_, failed, err := w.StartGoal(id, nil)
	return failed, err
}

// StartRace starts a draft goal once on each conduit of the pair. When fewer
// than two of them are up, it starts the goal once, on the one that is (or
// as usual), and says so in the note.
func (w *Work) StartRace(id int64) ([]Started, []string, string, error) {
	goal, err := w.Store.Goal(id)
	if err != nil || goal == nil {
		return nil, nil, "", cmp.Or(err, httpErr(404, "no goal %d", id))
	}
	if view, err := w.goalView(id); err != nil || view.State != "draft" || len(view.Tasks) > 0 {
		return nil, nil, "", cmp.Or(err, httpErr(409, "goal %d has already started", id))
	}
	pair := w.Races().Pair
	var up, down []string
	for _, name := range pair {
		if c := w.conduit(name); c != nil && c.Up(w.Agents) {
			up = append(up, name)
		} else {
			down = append(down, conduitLabel(name))
		}
	}
	if len(up) < 2 {
		note := "no conduit of the race was available, so it ran once as usual"
		if len(up) == 1 {
			note = fmt.Sprintf("only %s was available (%s was not), so it ran once on %s", conduitLabel(up[0]), strings.Join(down, " and "), conduitLabel(up[0]))
			_ = w.Store.SetGoalConduit(id, up[0])
		}
		started, failed, err := w.StartGoal(id, nil)
		if err == nil {
			_ = w.Store.raceStarted(id, nil, note)
			w.logf("goal %d: %s", id, note)
		}
		return started, failed, note, err
	}
	// Each lane is its own set of parts, recorded under its own conduit.
	var started []Started
	var failed []string
	for _, conduit := range up {
		var before int64
		_ = w.Store.db.QueryRow(`SELECT coalesce(max(id), 0) FROM tasks`).Scan(&before)
		parts, notStarted := w.startPartsOn(goal, goal.Repos, conduit)
		started, failed = append(started, parts...), append(failed, notStarted...)
		if _, err := w.Store.db.Exec(`INSERT INTO race_lanes (task_id, goal_id, conduit) SELECT id, goal_id, ? FROM tasks WHERE goal_id = ? AND id > ?`, conduit, id, before); err != nil {
			return started, failed, "", err
		}
	}
	return started, failed, "", w.Store.raceStarted(id, up, "")
}

// --- picking ---------------------------------------------------------------------------

// PickResult is what picking a race's winner did.
type PickResult struct {
	Pick      RacePick `json:"pick"`
	Merged    []string `json:"merged"`
	Discarded []string `json:"discarded"`
	Failed    []string `json:"failed"`
}

// PickWinner keeps the winner's branches, merging them unless keep is set,
// and removes the loser's worktrees and branches. "neither" removes both and
// closes the goal. The pick is kept with the goal's kind.
func (w *Work) PickWinner(id int64, winner string, merge bool) (*PickResult, error) {
	goal, err := w.Store.Goal(id)
	if err != nil || goal == nil {
		return nil, cmp.Or(err, httpErr(404, "no goal %d", id))
	}
	view, err := w.goalView(id)
	if err != nil {
		return nil, err
	}
	switch {
	case view.Racing == nil:
		return nil, httpErr(409, "goal %d did not race", id)
	case view.Racing.Pick != nil:
		return nil, httpErr(409, "the race of goal %d is already picked: %s", id, view.Racing.Pick.Winner)
	case winner != "neither" && !slices.Contains(view.Racing.Pair, winner):
		return nil, httpErr(400, "`winner` must be %s or neither", strings.Join(view.Racing.Pair, ", "))
	}
	for _, lane := range view.Racing.Lanes {
		if !laneReady(currentParts(lane.Tasks)) {
			return nil, httpErr(409, "the %s lane is still working: wait for it, or stop it first", lane.Label)
		}
	}
	result := &PickResult{Merged: []string{}, Discarded: []string{}, Failed: []string{}}
	for _, lane := range view.Racing.Lanes {
		for _, t := range currentParts(lane.Tasks) {
			if t.Agent == "" {
				continue
			}
			repo := cmp.Or(repoOf(t), "the goal")
			if lane.Conduit == winner {
				if !merge || t.Branch == nil || t.Merged {
					continue
				}
				if done, err := w.Agents.Merge(t.Agent); err != nil {
					result.Failed = append(result.Failed, fmt.Sprintf("Could not merge the %s branch in %s, kept for review: %v", lane.Label, repo, err))
				} else {
					result.Merged = append(result.Merged, repo+": "+done)
				}
				continue
			}
			if t.Branch == nil {
				continue // nothing was left of it: no changes
			}
			if done, err := w.Agents.RemoveWorktree(t.Agent, true); err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("Could not remove the %s branch in %s: %v", lane.Label, repo, err))
			} else {
				result.Discarded = append(result.Discarded, lane.Label+" in "+repo+": "+done)
			}
		}
	}
	result.Pick = RacePick{At: w.Store.ms(), Goal: id, Kind: w.GoalKind(*goal), Pair: view.Racing.Pair, Winner: winner}
	if _, err := w.Store.db.Exec(`INSERT INTO race_picks (at, goal_id, kind, pair, winner) VALUES (?, ?, ?, ?, ?)`,
		result.Pick.At, id, result.Pick.Kind, mustJSON(result.Pick.Pair), winner); err != nil {
		return nil, err
	}
	if winner == "neither" {
		if err := w.Store.SetGoalClosed(id, true); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// --- stats and suggestions ------------------------------------------------------------

// KindStats is how races of one kind went.
type KindStats struct {
	Kind string `json:"kind"`
	// Picks is every pick of this kind; Neither the ones no conduit won.
	Picks   int `json:"picks"`
	Neither int `json:"neither"`
	// Wins is each conduit's wins, and Raced how often it ran in a race of this kind.
	Wins  map[string]int `json:"wins"`
	Raced map[string]int `json:"raced"`
	// Suggested is the conduit a goal of this kind is suggested, once there are enough picks.
	Suggested string `json:"suggested,omitempty"`
}

// RaceStats is wins per kind per conduit.
type RaceStats struct {
	Conduits []string    `json:"conduits"`
	Kinds    []KindStats `json:"kinds"`
	Picks    int         `json:"picks"`
	MinPicks int         `json:"minPicks"`
}

// leader is the conduit with the most wins, "" on a tie or with too few decided picks.
func leader(stats KindStats, minPicks int) string {
	if stats.Picks-stats.Neither < minPicks {
		return ""
	}
	best, top, tie := "", 0, false
	for conduit, wins := range stats.Wins {
		switch {
		case wins > top:
			best, top, tie = conduit, wins, false
		case wins == top:
			tie = true
		}
	}
	if tie || top == 0 {
		return ""
	}
	return best
}

// Stats counts every pick, per kind and conduit.
func (w *Work) Stats() (RaceStats, error) {
	settings := w.Races()
	picks, err := w.Store.picks("")
	if err != nil {
		return RaceStats{}, err
	}
	stats := RaceStats{Conduits: slices.Clone(settings.Pair), Kinds: []KindStats{}, Picks: len(picks), MinPicks: settings.MinPicks}
	byKind := map[string]*KindStats{}
	for _, p := range picks {
		k := byKind[p.Kind]
		if k == nil {
			k = &KindStats{Kind: p.Kind, Wins: map[string]int{}, Raced: map[string]int{}}
			byKind[p.Kind] = k
		}
		k.Picks++
		for _, conduit := range p.Pair {
			k.Raced[conduit]++
			if !slices.Contains(stats.Conduits, conduit) {
				stats.Conduits = append(stats.Conduits, conduit)
			}
		}
		if p.Winner == "neither" {
			k.Neither++
		} else {
			k.Wins[p.Winner]++
		}
	}
	for _, k := range byKind {
		k.Suggested = leader(*k, settings.MinPicks)
		stats.Kinds = append(stats.Kinds, *k)
	}
	sort.Slice(stats.Kinds, func(i, j int) bool {
		if stats.Kinds[i].Picks != stats.Kinds[j].Picks {
			return stats.Kinds[i].Picks > stats.Kinds[j].Picks
		}
		return stats.Kinds[i].Kind < stats.Kinds[j].Kind
	})
	return stats, nil
}

// Suggest is the conduit that wins a kind, once it has enough picks; nil before.
func (w *Work) Suggest(kind string) *ConduitSuggestion {
	minPicks := w.Races().MinPicks
	picks, err := w.Store.picks(`WHERE kind = ?`, kind)
	if err != nil || len(picks) < minPicks {
		return nil
	}
	stats := KindStats{Kind: kind, Wins: map[string]int{}}
	for _, p := range picks {
		stats.Picks++
		if p.Winner == "neither" {
			stats.Neither++
		} else {
			stats.Wins[p.Winner]++
		}
	}
	best := leader(stats, minPicks)
	if best == "" || w.conduit(best) == nil {
		return nil
	}
	return &ConduitSuggestion{Conduit: best, Kind: kind, Wins: stats.Wins[best], Picks: stats.Picks - stats.Neither}
}

// --- routes ----------------------------------------------------------------------------

func (w *Work) conduitInfos() []ConduitInfo {
	on := w.conduits()
	infos := make([]ConduitInfo, len(on))
	for i, c := range on {
		infos[i] = ConduitInfo{Name: c.Name, Label: c.Label, Up: c.Up(w.Agents)}
	}
	return infos
}

func ownerOnly(rw http.ResponseWriter, r *http.Request, what string) bool {
	if part.Caller(r) != part.Owner {
		part.Error(rw, http.StatusForbidden, "only the owner can "+what)
		return false
	}
	return true
}

func (w *Work) mountRaces(p *part.Part) {
	goalID := func(rw http.ResponseWriter, r *http.Request) (int64, bool) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if goal, _ := w.Store.Goal(id); goal == nil {
			part.Error(rw, http.StatusNotFound, "no goal "+r.PathValue("id"))
			return 0, false
		}
		return id, true
	}
	p.Handle("PUT /v1/goals/{id}/race", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Race *bool `json:"race"`
		}
		if err := part.Decode(r, &body); err != nil || body.Race == nil {
			part.Error(rw, http.StatusBadRequest, "send `race`, true or false")
			return
		}
		if !ownerOnly(rw, r, "mark a goal to race") {
			return
		}
		id, ok := goalID(rw, r)
		if !ok {
			return
		}
		if view, _ := w.goalView(id); *body.Race && (view == nil || view.State != "draft") {
			part.Error(rw, http.StatusConflict, "only a draft goal can race")
			return
		}
		if err := w.Store.SetGoalRace(id, *body.Race); err != nil {
			fail(rw, err)
			return
		}
		answer(rw)(w.goalView(id))
	})
	p.Handle("PUT /v1/goals/{id}/conduit", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Conduit *string `json:"conduit"`
		}
		if err := part.Decode(r, &body); err != nil || body.Conduit == nil || (*body.Conduit != "" && w.conduit(*body.Conduit) == nil) {
			on := w.conduits()
			names := make([]string, len(on))
			for i, c := range on {
				names[i] = c.Name
			}
			part.Error(rw, http.StatusBadRequest, "`conduit` must be one of "+strings.Join(names, ", ")+", or \"\" for the default")
			return
		}
		id, ok := goalID(rw, r)
		if !ok {
			return
		}
		if err := w.Store.SetGoalConduit(id, *body.Conduit); err != nil {
			fail(rw, err)
			return
		}
		answer(rw)(w.goalView(id))
	})
	p.Handle("POST /v1/goals/{id}/race/pick", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Winner string `json:"winner"`
			Merge  *bool  `json:"merge"`
		}
		if err := part.Decode(r, &body); err != nil || body.Winner == "" {
			part.Error(rw, http.StatusBadRequest, "send `winner`: a conduit of the race, or neither")
			return
		}
		if !ownerOnly(rw, r, "pick a race's winner") {
			return
		}
		id, ok := goalID(rw, r)
		if !ok {
			return
		}
		result, err := w.PickWinner(id, body.Winner, body.Merge == nil || *body.Merge)
		if err != nil {
			fail(rw, err)
			return
		}
		view, _ := w.goalView(id)
		part.JSON(rw, http.StatusOK, map[string]any{"goal": view, "pick": result.Pick, "merged": result.Merged, "discarded": result.Discarded, "failed": result.Failed})
	})
	p.Handle("GET /v1/races", func(rw http.ResponseWriter, _ *http.Request) {
		views, err := w.GoalViews()
		if err != nil {
			fail(rw, err)
			return
		}
		races := []GoalView{}
		for _, view := range views {
			if view.Racing != nil {
				races = append(races, view)
			}
		}
		part.JSON(rw, http.StatusOK, map[string]any{"races": races})
	})
	p.Handle("GET /v1/races/stats", func(rw http.ResponseWriter, _ *http.Request) {
		answer(rw)(w.Stats())
	})
	p.Handle("GET /v1/settings/races", func(rw http.ResponseWriter, _ *http.Request) {
		part.JSON(rw, http.StatusOK, map[string]any{"pair": w.Races().Pair, "minPicks": w.Races().MinPicks, "conduits": w.conduitInfos()})
	})
	p.Handle("PATCH /v1/settings/races", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Pair     []string `json:"pair"`
			MinPicks *int     `json:"minPicks"`
		}
		if err := part.Decode(r, &body); err != nil || (body.Pair == nil && body.MinPicks == nil) {
			part.Error(rw, http.StatusBadRequest, "send `pair` and/or `minPicks`")
			return
		}
		if !ownerOnly(rw, r, "change the race settings") {
			return
		}
		settings := w.Races()
		if body.Pair != nil {
			if len(body.Pair) != 2 || body.Pair[0] == body.Pair[1] || w.conduit(body.Pair[0]) == nil || w.conduit(body.Pair[1]) == nil {
				part.Error(rw, http.StatusBadRequest, "`pair` must be two different conduits that are turned on here")
				return
			}
			settings.Pair = body.Pair
		}
		if body.MinPicks != nil {
			if *body.MinPicks < 1 || *body.MinPicks > 1000 {
				part.Error(rw, http.StatusBadRequest, "`minPicks` must be 1 to 1000")
				return
			}
			settings.MinPicks = *body.MinPicks
		}
		if err := w.Store.SetSetting("races", settings); err != nil {
			fail(rw, err)
			return
		}
		part.JSON(rw, http.StatusOK, map[string]any{"pair": settings.Pair, "minPicks": settings.MinPicks, "conduits": w.conduitInfos()})
	})
}
