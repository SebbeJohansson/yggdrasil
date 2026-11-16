// ygg runs one part of Yggdrasil per process: `ygg gjallar`, `ygg ratatoskr`…
//
// One binary, one process per part. Each part is its own service with its
// own port, token, database and timers, so one part crashing, hanging or
// being redeployed never takes another with it, and a machine runs only the
// parts it needs. See docs/architecture.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"yggdrasil/api"
	"yggdrasil/dellingr"
	"yggdrasil/gjallar"
	"yggdrasil/hermodr"
	"yggdrasil/huginn"
	"yggdrasil/internal/contract"
	"yggdrasil/internal/migrate"
	"yggdrasil/internal/part"
	"yggdrasil/internal/where"
	"yggdrasil/munin"
	"yggdrasil/norn"
	"yggdrasil/ratatoskr"
	"yggdrasil/sindri"
	"yggdrasil/vedrfolnir"
)

var parts = map[string]struct {
	about string
	main  func(context.Context) error
}{
	"norn":       {"assistant: inbox, calendars, memory, the front desk", norn.Main},
	"sindri":     {"coding: agents, worktrees, review", sindri.Main},
	"gjallar":    {"alerts and warnings from every part", gjallar.Main},
	"ratatoskr":  {"watchdog: is each part alive and doing its job", ratatoskr.Main},
	"munin":      {"checks the systems outside Yggdrasil that the parts depend on", munin.Main},
	"vedrfolnir": {"reports on the machine: CPU, GPU, disks, services, containers", vedrfolnir.Main},
	"huginn":     {"reacts to what just happened: messages, meetings, alerts", huginn.Main},
	"dellingr":   {"runs the jobs you put on a clock", dellingr.Main},
	"hermodr":    {"carries every message for you to the widget and your phone", hermodr.Main},
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ygg <part>    run one part of Yggdrasil")
	fmt.Fprintln(os.Stderr, "       ygg check <part> [url]   check a running part, built-in or not, against its contract")
	fmt.Fprintln(os.Stderr, "       ygg migrate from-my-agent|to-my-agent [--replace]   move your data between my-agent and Yggdrasil")
	fmt.Fprintln(os.Stderr, "       ygg version")
	fmt.Fprintln(os.Stderr, "\nparts:")
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %-11s %s\n", name, parts[name].about)
	}
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "migrate" {
		os.Exit(migrateCommand(os.Args[2:]))
	}
	if len(os.Args) >= 3 && os.Args[1] == "check" {
		os.Exit(check(os.Args[2:]))
	}
	if len(os.Args) != 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	if name == "version" {
		fmt.Println(part.Version)
		return
	}
	found, ok := parts[name]
	if !ok {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := found.main(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ygg %s: %v\n", name, err)
		os.Exit(1)
	}
}

// check runs a part's contract against a running service: by default the one
// at YGG_<PART>_URL. The owner token is YGG_TOKEN, or
// ~/.config/yggdrasil/owner.token; YGG_SENDER_TOKEN acts as a part where the
// contract has steps for one (Gjallar: raise and resolve a test alert).
func check(args []string) int {
	partName := args[0]
	spec := api.Spec(partName)
	if spec == nil {
		fmt.Fprintf(os.Stderr, "no contract for %q; there are: %s\n", partName, strings.Join(api.Parts(), ", "))
		return 2
	}
	loaded, err := contract.Load(partName, spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	url := where.URL(partName)
	if len(args) > 1 {
		url = args[1]
	}
	owner := os.Getenv("YGG_TOKEN")
	if owner == "" {
		home, _ := os.UserHomeDir()
		if data, err := os.ReadFile(filepath.Join(home, ".config/yggdrasil/owner.token")); err == nil {
			owner = strings.TrimSpace(string(data))
		}
	}
	fmt.Printf("Checking %s at %s against api/%s.yaml\n", partName, url, partName)
	results := contract.Check(context.Background(), loaded, url, contract.Options{OwnerToken: owner, SenderToken: os.Getenv("YGG_SENDER_TOKEN")})
	for _, result := range results {
		switch {
		case result.Skip:
			fmt.Printf("  skip  %s (%s)\n", result.Name, result.Detail)
		case result.OK:
			fmt.Printf("  pass  %s\n", result.Name)
		default:
			fmt.Printf("  FAIL  %s: %s\n", result.Name, result.Detail)
		}
	}
	if !contract.Passed(results) {
		fmt.Println("Does not meet the contract.")
		return 1
	}
	fmt.Println("Meets the contract.")
	return 0
}

// migrateCommand moves your data between my-agent and Yggdrasil. Stop both
// first: the databases must not change while they move.
func migrateCommand(args []string) int {
	direction := args[0]
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	myAgent := flags.String("my-agent", filepath.Join(home, "projects/my-agent"), "my-agent's checkout (its data/ folder holds agent.db and the sign-ins)")
	runner := flags.String("runner", filepath.Join(home, ".local/state/my-agent-runner/agents.json"), "the runner's agents")
	ygg := flags.String("yggdrasil", filepath.Join(home, "projects/yggdrasil"), "Yggdrasil's checkout (its data/ folder holds norn.db and the sign-ins)")
	state := flags.String("state", filepath.Join(home, ".local/state/yggdrasil"), "where Sindri keeps sindri.db and its agents")
	replace := flags.Bool("replace", false, "empty what the target already holds first (it is backed up)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	paths := migrate.Paths{
		AgentDB: filepath.Join(*myAgent, "data", "agent.db"), AgentData: filepath.Join(*myAgent, "data"), RunnerState: *runner,
		NornDB: filepath.Join(*ygg, "data", "norn.db"), NornData: filepath.Join(*ygg, "data"),
		SindriDB: filepath.Join(*state, "sindri.db"), SindriState: filepath.Join(*state, "sindri-agents.json"),
	}
	var report *migrate.Report
	var err error
	switch direction {
	case "from-my-agent":
		report, err = migrate.FromMyAgent(paths, migrate.Options{Replace: *replace})
	case "to-my-agent":
		report, err = migrate.ToMyAgent(paths, migrate.Options{Replace: *replace})
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ygg migrate:", err)
		return 1
	}
	for _, path := range report.Backup {
		fmt.Println("backed up", path)
	}
	names := make([]string, 0, len(report.Rows))
	for name := range report.Rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-22s %d\n", name, report.Rows[name])
	}
	for _, file := range report.Files {
		fmt.Println("wrote", file)
	}
	return 0
}
