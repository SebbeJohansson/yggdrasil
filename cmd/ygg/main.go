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
	"sort"
	"syscall"

	"yggdrasil/gjallar"
	"yggdrasil/internal/part"
	"yggdrasil/munin"
	"yggdrasil/ratatoskr"
	"yggdrasil/vedrfolnir"
)

var parts = map[string]struct {
	about string
	main  func(context.Context) error
}{
	"gjallar":    {"alerts and warnings from every part", gjallar.Main},
	"ratatoskr":  {"watchdog: is each part alive and doing its job", ratatoskr.Main},
	"munin":      {"checks the systems outside Yggdrasil that the parts depend on", munin.Main},
	"vedrfolnir": {"reports on the machine: CPU, GPU, disks, services, containers", vedrfolnir.Main},
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ygg <part>    run one part of Yggdrasil")
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
