package main

// The commands that ask the running parts how things are, for a terminal and
// for coding agents: `ygg status`, `ygg alerts`, `ygg gossip`, `ygg findings`,
// `ygg agents`, `ygg strikes` and `ygg strike <part>`. Each finds the part at
// YGG_<PART>_URL (or its default port) with the owner token (YGG_TOKEN, or
// ~/.config/yggdrasil/owner.token), and prints plain lines; `--json` prints
// what the part answered instead. Only `ygg strike` changes anything, and it
// asks first unless given --yes.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"yggdrasil/internal/call"
	"yggdrasil/internal/where"
)

// tellCommands are the commands of this file, with what each says.
var tellCommands = map[string]struct {
	about string
	run   func(args []string) int
}{
	"status":   {"each part up or down, loops that are late, and Gjallar's open alerts", status},
	"alerts":   {"Gjallar's open alerts, the most recent first", alerts},
	"gossip":   {"the parts' latest gossip in Huginn [n exchanges, default 3]", gossip},
	"findings": {"Heimdall's open findings with their fixes [--all for the small ones too]", findings},
	"usage":    {"Claude usage by day, goal and caller [--since 2026-11-01, default 7 days]", usageCommand},
	"agents":   {"Sindri's agents that are not done, and the Claude Code sessions running", agents},
	"strikes":  {"Loki's last strikes and who noticed", strikes},
	"strike":   {"have Loki trick a part: ygg strike <part> [health|loop|slow|...] [--loop name] [--minutes n] [--yes]", strike},
}

// out is where the commands write; tests swap it.
var out io.Writer = os.Stdout

// in is where `ygg strike` reads its answer; tests swap it.
var in io.Reader = os.Stdin

// now is the clock; tests fix it.
var now = time.Now

func ownerToken() string {
	if token := strings.TrimSpace(os.Getenv("YGG_TOKEN")); token != "" {
		return token
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".config/yggdrasil/owner.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func partClient(name string) *call.Part {
	return call.New(name, where.URL(name), ownerToken(), 8*time.Second)
}

// read asks a part for a route and decodes the answer into into.
func read(name, path string, into any) error {
	return partClient(name).Do(context.Background(), "GET", path, nil, into)
}

// flags parses a command's flags wherever they are among its words, and
// returns the words that are not flags.
func flags(set *flag.FlagSet, args []string) ([]string, error) {
	set.SetOutput(io.Discard)
	var words, given []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			words = append(words, arg)
			continue
		}
		given = append(given, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		// A flag that takes a value takes the next word.
		if f := set.Lookup(name); f != nil {
			if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool && i+1 < len(args) {
				i++
				given = append(given, args[i])
			}
		}
	}
	return words, set.Parse(given)
}

func printJSON(value any) int {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func failed(command string, err error) int {
	fmt.Fprintf(os.Stderr, "ygg %s: %v\n", command, err)
	return 1
}

// ago says how long ago t was, short: "40s", "12m", "3h", "2d".
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func clip(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) <= max {
		return text
	}
	return string([]rune(text)[:max-1]) + "…"
}

// --- ygg status ------------------------------------------------------------------

type watchedPart struct {
	Name   string `json:"name"`
	Up     bool   `json:"up"`
	Error  string `json:"error"`
	Health *struct {
		Loops map[string]struct {
			EverySeconds int       `json:"everySeconds"`
			LastRunAt    time.Time `json:"lastRunAt"`
			LastError    string    `json:"lastError"`
		} `json:"loops"`
	} `json:"health"`
}

type alert struct {
	ID         int       `json:"id"`
	Source     string    `json:"source"`
	Key        string    `json:"key"`
	Severity   string    `json:"severity"`
	Title      string    `json:"title"`
	State      string    `json:"state"`
	Count      int       `json:"count"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Muted      bool      `json:"muted"`
}

// partTrouble is what is wrong with an up part: loops that failed their last
// run or are more than three runs late. Empty when nothing is.
func partTrouble(part watchedPart) []string {
	if part.Health == nil {
		return nil
	}
	var trouble []string
	names := make([]string, 0, len(part.Health.Loops))
	for name := range part.Health.Loops {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		loop := part.Health.Loops[name]
		switch {
		case loop.LastError != "":
			trouble = append(trouble, fmt.Sprintf("%s loop failing: %s", name, clip(loop.LastError, 60)))
		case loop.EverySeconds > 0 && !loop.LastRunAt.IsZero() && now().Sub(loop.LastRunAt) > 3*time.Duration(loop.EverySeconds)*time.Second:
			trouble = append(trouble, fmt.Sprintf("%s loop late, last ran %s ago", name, ago(loop.LastRunAt)))
		}
	}
	return trouble
}

func status(args []string) int {
	set := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what the parts answered")
	if _, err := flags(set, args); err != nil {
		return failed("status", err)
	}
	var watched struct {
		Parts     []watchedPart `json:"parts"`
		GjallarUp bool          `json:"gjallarUp"`
	}
	if err := read("ratatoskr", "/v1/status", &watched); err != nil {
		return failed("status", fmt.Errorf("Ratatoskr is the one who knows, and %w", err))
	}
	var open struct {
		Alerts []alert `json:"alerts"`
	}
	alertsErr := read("gjallar", "/v1/alerts?state=open&limit=200", &open)
	if *asJSON {
		return printJSON(map[string]any{"parts": watched.Parts, "alerts": open.Alerts})
	}
	sort.Slice(watched.Parts, func(i, j int) bool { return watched.Parts[i].Name < watched.Parts[j].Name })
	up := 0
	for _, part := range watched.Parts {
		if part.Up {
			up++
		}
	}
	fmt.Fprintf(out, "Yggdrasil  %d of %d parts up\n", up, len(watched.Parts))
	for _, part := range watched.Parts {
		switch trouble := partTrouble(part); {
		case !part.Up:
			fmt.Fprintf(out, "  ✗ %-11s down: %s\n", part.Name, clip(part.Error, 70))
		case len(trouble) > 1:
			fmt.Fprintf(out, "  ! %-11s up, %s (+%d more)\n", part.Name, trouble[0], len(trouble)-1)
		case len(trouble) == 1:
			fmt.Fprintf(out, "  ! %-11s up, %s\n", part.Name, trouble[0])
		default:
			fmt.Fprintf(out, "  ✓ %-11s up\n", part.Name)
		}
	}
	switch {
	case alertsErr != nil:
		fmt.Fprintf(out, "Gjallar: can't say (%v)\n", alertsErr)
	case len(open.Alerts) == 0:
		fmt.Fprintln(out, "Gjallar: no open alerts")
	default:
		fmt.Fprintf(out, "Gjallar: %d open %s (ygg alerts)\n", len(open.Alerts), map[bool]string{true: "alert", false: "alerts"}[len(open.Alerts) == 1])
	}
	if up < len(watched.Parts) {
		return 1
	}
	return 0
}

// --- ygg alerts ------------------------------------------------------------------

func alerts(args []string) int {
	set := flag.NewFlagSet("alerts", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Gjallar answered")
	if _, err := flags(set, args); err != nil {
		return failed("alerts", err)
	}
	var open struct {
		Alerts []alert `json:"alerts"`
	}
	if err := read("gjallar", "/v1/alerts?limit=200", &open); err != nil {
		return failed("alerts", err)
	}
	if *asJSON {
		return printJSON(open.Alerts)
	}
	if len(open.Alerts) == 0 {
		fmt.Fprintln(out, "No open alerts.")
		return 0
	}
	for _, a := range open.Alerts {
		muted := ""
		if a.Muted {
			muted = " (muted)"
		}
		times := ""
		if a.Count > 1 {
			times = fmt.Sprintf(" ×%d", a.Count)
		}
		fmt.Fprintf(out, "%-8s %-10s %s%s%s  %s ago  #%d\n", a.Severity, a.Source, clip(a.Title, 80), times, muted, ago(a.LastSeenAt), a.ID)
	}
	return 0
}

// --- ygg gossip ------------------------------------------------------------------

type gossipLine struct {
	ID       int       `json:"id"`
	Exchange string    `json:"exchange"`
	At       time.Time `json:"at"`
	Name     string    `json:"name"`
	Text     string    `json:"text"`
	About    []string  `json:"about"`
}

// lastExchanges is the last n exchanges of lines, oldest first, each its lines in order.
func lastExchanges(lines []gossipLine, n int) [][]gossipLine {
	var exchanges [][]gossipLine
	for _, line := range lines {
		if last := len(exchanges) - 1; last >= 0 && exchanges[last][0].Exchange == line.Exchange {
			exchanges[last] = append(exchanges[last], line)
			continue
		}
		exchanges = append(exchanges, []gossipLine{line})
	}
	if len(exchanges) > n {
		exchanges = exchanges[len(exchanges)-n:]
	}
	return exchanges
}

func gossip(args []string) int {
	set := flag.NewFlagSet("gossip", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Huginn answered")
	words, err := flags(set, args)
	if err != nil {
		return failed("gossip", err)
	}
	n := 3
	if len(words) > 0 {
		if _, err := fmt.Sscan(words[0], &n); err != nil || n < 1 {
			return failed("gossip", fmt.Errorf("how many exchanges? %q is not a number", words[0]))
		}
	}
	var said struct {
		On    bool         `json:"on"`
		Lines []gossipLine `json:"lines"`
	}
	if err := read("huginn", "/v1/gossip", &said); err != nil {
		return failed("gossip", err)
	}
	exchanges := lastExchanges(said.Lines, n)
	if *asJSON {
		return printJSON(exchanges)
	}
	if !said.On {
		fmt.Fprintln(out, "The parts are keeping to themselves: gossip is off.")
	}
	if len(exchanges) == 0 {
		fmt.Fprintln(out, "Nothing to gossip about yet.")
		return 0
	}
	for i, exchange := range exchanges {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "— %s ago, about: %s\n", ago(exchange[0].At), strings.Join(exchange[0].About, " "))
		for _, line := range exchange {
			fmt.Fprintf(out, "  %-10s %s\n", line.Name, line.Text)
		}
	}
	return 0
}

// --- ygg findings ------------------------------------------------------------------

type finding struct {
	ID         string `json:"id"`
	Check      string `json:"check"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Where      string `json:"where"`
	Fix        string `json:"fix"`
	Repo       string `json:"repo"`
	ResolvedAt string `json:"resolvedAt"`
}

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

// openFindings is the findings still open, high first, the low ones only when all is set.
func openFindings(list []finding, all bool) []finding {
	var open []finding
	for _, f := range list {
		if f.ResolvedAt != "" || (!all && f.Severity == "low") {
			continue
		}
		open = append(open, f)
	}
	sort.SliceStable(open, func(i, j int) bool { return severityRank[open[i].Severity] < severityRank[open[j].Severity] })
	return open
}

func findings(args []string) int {
	set := flag.NewFlagSet("findings", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Heimdall answered")
	all := set.Bool("all", false, "the small ones too")
	if _, err := flags(set, args); err != nil {
		return failed("findings", err)
	}
	var report struct {
		CheckedAt string    `json:"checkedAt"`
		Findings  []finding `json:"findings"`
	}
	if err := read("heimdall", "/v1/report", &report); err != nil {
		return failed("findings", err)
	}
	open := openFindings(report.Findings, *all)
	if *asJSON {
		return printJSON(open)
	}
	if len(open) == 0 {
		if *all {
			fmt.Fprintln(out, "Heimdall has nothing open.")
		} else {
			fmt.Fprintln(out, "Nothing high or medium open. (ygg findings --all for the small ones)")
		}
		return 0
	}
	for _, f := range open {
		where := ""
		if f.Where != "" {
			where = "  " + f.Where
		}
		fmt.Fprintf(out, "%-6s %-9s %s%s\n", f.Severity, f.Check, clip(f.Title, 90), where)
		if f.Fix != "" {
			fmt.Fprintf(out, "       fix: %s\n", clip(f.Fix, 100))
		}
	}
	if !*all {
		small := len(openFindings(report.Findings, true)) - len(open)
		if small > 0 {
			fmt.Fprintf(out, "and %d small ones (ygg findings --all)\n", small)
		}
	}
	return 0
}

// --- ygg agents --------------------------------------------------------------------

type sindriAgent struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Status       string `json:"status"`
	Cwd          string `json:"cwd"`
	LastActivity int64  `json:"lastActivity"`
	AsksYou      bool   `json:"asksYou"`
}

type claudeSession struct {
	SessionID    string `json:"sessionId"`
	Directory    string `json:"directory"`
	Name         string `json:"name"`
	LastActivity int64  `json:"lastActivity"`
	State        struct {
		Phase string `json:"phase"`
	} `json:"state"`
}

// liveAgents is Sindri's agents that are not over, the ones needing you first.
func liveAgents(list []sindriAgent) []sindriAgent {
	order := map[string]int{"needs-approval": 0, "waiting": 1, "working": 2, "starting": 3, "failed": 4}
	var live []sindriAgent
	for _, a := range list {
		if _, ok := order[a.Status]; ok {
			live = append(live, a)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return order[live[i].Status] < order[live[j].Status] })
	return live
}

func agents(args []string) int {
	set := flag.NewFlagSet("agents", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Sindri answered")
	if _, err := flags(set, args); err != nil {
		return failed("agents", err)
	}
	var list []sindriAgent
	if err := read("sindri", "/v1/agents", &list); err != nil {
		return failed("agents", err)
	}
	var sessions struct {
		Claude struct {
			Sessions []claudeSession `json:"sessions"`
		} `json:"claude"`
	}
	sessionsErr := read("sindri", "/v1/sessions", &sessions)
	live := liveAgents(list)
	if *asJSON {
		return printJSON(map[string]any{"agents": live, "sessions": sessions.Claude.Sessions})
	}
	fmt.Fprintln(out, "Sindri's agents")
	if len(live) == 0 {
		fmt.Fprintln(out, "  none running")
	}
	for _, a := range live {
		fmt.Fprintf(out, "  %-15s %s  (%s, %s ago)\n", a.Status, clip(a.Label, 70), filepath.Base(a.Cwd), ago(time.UnixMilli(a.LastActivity)))
	}
	fmt.Fprintln(out, "Claude Code sessions")
	switch {
	case sessionsErr != nil:
		fmt.Fprintf(out, "  can't say (%v)\n", sessionsErr)
	case len(sessions.Claude.Sessions) == 0:
		fmt.Fprintln(out, "  none running")
	}
	for _, s := range sessions.Claude.Sessions {
		phase := s.State.Phase
		if phase == "" {
			phase = "unknown"
		}
		fmt.Fprintf(out, "  %-15s %s  (%s, %s ago)\n", phase, clip(s.Name, 70), filepath.Base(s.Directory), ago(time.UnixMilli(s.LastActivity)))
	}
	return 0
}

// --- ygg strikes / ygg strike ----------------------------------------------------------

type trick struct {
	Via    string   `json:"via"`
	Kind   string   `json:"kind"`
	Loop   string   `json:"loop,omitempty"`
	Label  string   `json:"label,omitempty"`
	Expect []string `json:"expect,omitempty"`
}

type target struct {
	Name string `json:"name"`
	Up   bool   `json:"up"`
	Vias []struct {
		Via       string `json:"via"`
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	} `json:"vias"`
	Tricks []trick `json:"tricks"`
}

type lokiStrike struct {
	ID        string    `json:"id"`
	Part      string    `json:"part"`
	Trick     trick     `json:"trick"`
	By        string    `json:"by"`
	StartedAt time.Time `json:"startedAt"`
	EndsAt    time.Time `json:"endsAt"`
	State     string    `json:"state"`
	Checks    []struct {
		Watcher   string `json:"watcher"`
		NoticedAt string `json:"noticedAt"`
		OK        *bool  `json:"ok"`
	} `json:"checks"`
}

// pickTrick is the trick to play on a part: one of the kind asked for (and
// loop, when given) by a way that is open, health when no kind is given.
func pickTrick(targets []target, part, kind, loop string) (trick, error) {
	for _, t := range targets {
		if t.Name != part {
			continue
		}
		if !t.Up {
			return trick{}, fmt.Errorf("%s is down already", part)
		}
		open := map[string]bool{}
		for _, via := range t.Vias {
			open[via.Via] = via.Available
		}
		if kind == "" {
			kind = "health"
		}
		var kinds []string
		for _, tr := range t.Tricks {
			if !open[tr.Via] {
				continue
			}
			kinds = append(kinds, tr.Kind+map[bool]string{true: ":" + tr.Loop, false: ""}[tr.Loop != ""])
			if tr.Kind == kind && (loop == "" || tr.Loop == loop) {
				return tr, nil
			}
		}
		if len(kinds) == 0 {
			return trick{}, fmt.Errorf("Loki can't reach %s any way right now", part)
		}
		return trick{}, fmt.Errorf("no %s trick for %s; it can do: %s", kind, part, strings.Join(kinds, ", "))
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	return trick{}, fmt.Errorf("Loki doesn't know %q; it knows: %s", part, strings.Join(names, ", "))
}

func strikeVerdict(s lokiStrike) string {
	var noticed, missed []string
	for _, c := range s.Checks {
		switch {
		case c.NoticedAt != "" || (c.OK != nil && *c.OK):
			noticed = append(noticed, c.Watcher)
		case c.OK != nil && !*c.OK:
			missed = append(missed, c.Watcher)
		}
	}
	parts := []string{s.State}
	if len(noticed) > 0 {
		parts = append(parts, "noticed by "+strings.Join(noticed, ", "))
	}
	if len(missed) > 0 {
		parts = append(parts, "missed by "+strings.Join(missed, ", "))
	}
	return strings.Join(parts, "; ")
}

func strikes(args []string) int {
	set := flag.NewFlagSet("strikes", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Loki answered")
	if _, err := flags(set, args); err != nil {
		return failed("strikes", err)
	}
	var last struct {
		Strikes []lokiStrike `json:"strikes"`
	}
	if err := read("loki", "/v1/strikes?limit=5", &last); err != nil {
		return failed("strikes", err)
	}
	if *asJSON {
		return printJSON(last.Strikes)
	}
	if len(last.Strikes) == 0 {
		fmt.Fprintln(out, "Loki has played no tricks lately.")
		return 0
	}
	for _, s := range last.Strikes {
		label := s.Trick.Kind
		if s.Trick.Loop != "" {
			label += " " + s.Trick.Loop
		}
		fmt.Fprintf(out, "%-10s %-14s %s ago  %s\n", s.Part, label, ago(s.StartedAt), strikeVerdict(s))
	}
	return 0
}

func strike(args []string) int {
	set := flag.NewFlagSet("strike", flag.ContinueOnError)
	loop := set.String("loop", "", "which loop, for a loop trick")
	minutes := set.Int("minutes", 3, "how long, 1 to 10")
	yes := set.Bool("yes", false, "don't ask first")
	asJSON := set.Bool("json", false, "print what Loki answered")
	words, err := flags(set, args)
	if err != nil {
		return failed("strike", err)
	}
	if len(words) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ygg strike <part> [kind] [--loop name] [--minutes n] [--yes]")
		return 2
	}
	part, kind := words[0], ""
	if len(words) > 1 {
		kind = words[1]
	}
	var targets struct {
		Parts []target `json:"parts"`
	}
	if err := read("loki", "/v1/targets", &targets); err != nil {
		return failed("strike", err)
	}
	chosen, err := pickTrick(targets.Parts, part, kind, *loop)
	if err != nil {
		return failed("strike", err)
	}
	if !*yes {
		fmt.Fprintf(out, "Loki will %s for %d min (%s). Go? [y/N] ", strings.ToLower(chosen.Label[:1])+chosen.Label[1:], *minutes, chosen.Via)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
			fmt.Fprintln(out, "Called off.")
			return 0
		}
	}
	body := map[string]any{"part": part, "trick": map[string]any{"via": chosen.Via, "kind": chosen.Kind, "loop": chosen.Loop}, "minutes": *minutes}
	var started lokiStrike
	if err := partClient("loki").Do(context.Background(), "POST", "/v1/strikes", body, &started); err != nil {
		return failed("strike", err)
	}
	if *asJSON {
		return printJSON(started)
	}
	fmt.Fprintf(out, "Loki strikes %s until %s. Watch who notices: ygg strikes\n", part, started.EndsAt.Local().Format("15:04:05"))
	return 0
}

// usageCommand prints Sindri's Claude usage ledger.
func usageCommand(args []string) int {
	set := flag.NewFlagSet("usage", flag.ContinueOnError)
	asJSON := set.Bool("json", false, "print what Sindri answered")
	since := set.String("since", "", "a date or RFC 3339 time (default 7 days ago)")
	if _, err := flags(set, args); err != nil {
		return failed("usage", err)
	}
	path := "/v1/usage"
	if *since != "" {
		path += "?since=" + url.QueryEscape(*since)
	}
	var report struct {
		Total    usageTotal   `json:"total"`
		ByDay    []usageTotal `json:"byDay"`
		ByGoal   []usageTotal `json:"byGoal"`
		ByCaller []usageTotal `json:"byCaller"`
		Window   struct {
			Hours  float64 `json:"hours"`
			Tokens int64   `json:"tokens"`
			Budget int64   `json:"budget"`
			Share  float64 `json:"share"`
		} `json:"window"`
	}
	if err := read("sindri", path, &report); err != nil {
		return failed("usage", err)
	}
	if *asJSON {
		return printJSON(report)
	}
	line := func(t usageTotal, key string) {
		fmt.Fprintf(out, "  %-18s %5d calls  %9d in  %8d out  ≈$%.2f\n", key, t.Calls, t.TokensIn, t.TokensOut, t.CostUSD)
	}
	fmt.Fprintln(out, "Claude usage")
	line(report.Total, "total")
	if report.Window.Budget > 0 {
		fmt.Fprintf(out, "  last %gh: %d of %d tokens (%.0f%%)\n", report.Window.Hours, report.Window.Tokens, report.Window.Budget, report.Window.Share*100)
	} else {
		fmt.Fprintf(out, "  last %gh: %d tokens (no budget set: SINDRI_USAGE_BUDGET)\n", report.Window.Hours, report.Window.Tokens)
	}
	for _, group := range []struct {
		title string
		rows  []usageTotal
	}{{"By day", report.ByDay}, {"By goal", report.ByGoal}, {"By caller", report.ByCaller}} {
		fmt.Fprintln(out, group.title)
		if len(group.rows) == 0 {
			fmt.Fprintln(out, "  nothing yet")
		}
		for _, row := range group.rows {
			key := row.Key
			if key == "" {
				key = "no goal"
			} else if group.title == "By goal" {
				key = "goal " + key
			}
			line(row, key)
		}
	}
	return 0
}

type usageTotal struct {
	Key       string  `json:"key"`
	Calls     int     `json:"calls"`
	TokensIn  int64   `json:"tokensIn"`
	TokensOut int64   `json:"tokensOut"`
	CostUSD   float64 `json:"costUsd"`
}
