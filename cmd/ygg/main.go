// ygg runs one part of Yggdrasil per process: `ygg gjallar`, `ygg ratatoskr`…
//
// One binary, one process per part. Each part is its own service with its
// own port, token, database and timers, so one part crashing, hanging or
// being redeployed never takes another with it, and a machine runs only the
// parts it needs. See docs/architecture.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"yggdrasil/api"
	"yggdrasil/gjallar"
	"yggdrasil/internal/contract"
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
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ygg <part>    run one part of Yggdrasil")
	fmt.Fprintln(os.Stderr, "       ygg check <part> [url]   check a running part, built-in or not, against its contract")
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
