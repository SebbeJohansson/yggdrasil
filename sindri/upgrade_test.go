package sindri

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"yggdrasil/internal/part"
)

func TestAnUpgradeRunsDetachedAndReportsHowItWent(t *testing.T) {
	was := part.Version
	t.Cleanup(func() { part.Version = was })
	var started []string
	u := &Upgrader{Dir: t.TempDir(), Run: func(argv []string) error { started = argv; return nil }}

	part.Version = "9f1c2ab-dirty"
	if _, err := u.Start("v0.3.4"); err == nil || !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("a machine built from a checkout was upgraded: %v", err)
	}

	part.Version = "v0.3.0"
	if _, err := u.Start("nonsense"); err == nil {
		t.Fatal("a version that is not one was accepted")
	}
	if _, err := u.Start("v0.3.4"); err != nil {
		t.Fatal(err)
	}
	if last := u.Status(); last == nil || !last.Running || last.Version != "v0.3.4" || last.From != "v0.3.0" {
		t.Fatalf("status while running: %+v", last)
	}
	if _, err := u.Start("v0.3.4"); err == nil {
		t.Fatal("a second upgrade started while one runs")
	}

	// Run what was started, with a stand-in for ygg.
	fake := t.TempDir() + "/ygg"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"upgrading to $2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started[3] = fake
	if out, err := exec.Command(started[0], started[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	last := u.Status()
	if last.Running || last.OK == nil || !*last.OK || !strings.Contains(last.Log, "upgrading to v0.3.4") {
		t.Fatalf("status once finished: %+v", last)
	}
}
