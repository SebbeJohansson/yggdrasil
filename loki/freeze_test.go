package loki

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sleeper is a child process to freeze: `sleep 60`, killed when the test ends.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep here: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// stopped is whether the process is stopped (state T in /proc/<pid>/stat).
func stopped(t *testing.T, pid int) bool {
	t.Helper()
	text, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatalf("reading pid %d: %v", pid, err)
	}
	fields := strings.Fields(string(text[strings.LastIndexByte(string(text), ')')+1:]))
	return fields[0] == "T" || fields[0] == "t"
}

func eventually(t *testing.T, within time.Duration, ok func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ok()
}

func TestFindProcessesSeesOnlyWhatLokiMaySignal(t *testing.T) {
	pid := sleeper(t)
	proc := t.TempDir()
	write := func(pid int, args ...string) {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o600)
	}
	write(pid, "/repo/bin/dev-container/ygg", "norn")
	write(os.Getpid(), "ygg", "loki")
	write(1, "ygg", "norn")
	_ = os.MkdirAll(filepath.Join(proc, "self"), 0o700)

	if pids, why := FindProcesses(proc, "norn"); len(pids) != 1 || pids[0] != pid || why != "" {
		t.Errorf("norn: %v %q", pids, why)
	}
	if pids, why := FindProcesses(proc, "loki"); len(pids) != 0 || !strings.Contains(why, "never freezes itself") {
		t.Errorf("loki: %v %q", pids, why)
	}
	if pids, why := FindProcesses(proc, "sindri"); len(pids) != 0 || !strings.Contains(why, "can't see Sindri's process") {
		t.Errorf("sindri: %v %q", pids, why)
	}
	write(pid+100000, "ygg", "huginn") // no such process: not Loki's to signal
	if pids, why := FindProcesses(proc, "huginn"); len(pids) != 0 || why == "" {
		t.Errorf("huginn: %v %q", pids, why)
	}
}

func TestAFreezeStopsTheProcessAndThawsItAtUntil(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.Minute = 400 * time.Millisecond
	l.HealWait = 100 * time.Millisecond
	pid := sleeper(t)
	l.processes = func(name string) ([]int, string) {
		if name == "norn" {
			return []int{pid}, ""
		}
		return nil, "not here"
	}
	strike, err := l.Start(context.Background(), "norn", Trick{Kind: KindFreeze}, 1, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if strike.Trick.Via != ViaFreeze || len(strike.Pids) != 1 || strike.Pids[0] != pid {
		t.Fatalf("strike: %+v", strike)
	}
	if !eventually(t, time.Second, func() bool { return stopped(t, pid) }) {
		t.Error("not frozen")
	}
	if got := strings.Join(ExpectStrings("norn", strike.Trick), ","); got != "ratatoskr:down:norn,gjallar:alert" {
		t.Errorf("expect: %s", got)
	}
	l.Wait()
	if !eventually(t, time.Second, func() bool { return !stopped(t, pid) }) {
		t.Error("still frozen after until")
	}
	if len(l.frozen) != 0 {
		t.Errorf("still held: %v", l.frozen)
	}
}

func TestAStoppedFreezeThawsAtOnceAndCallsTheDetachedThawOff(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.Minute = time.Minute
	pid := sleeper(t)
	l.processes = func(string) ([]int, string) { return []int{pid}, "" }
	strike, err := l.Start(context.Background(), "norn", Trick{Via: ViaFreeze, Kind: KindFreeze}, 5, "manual")
	if err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	thaw := l.frozen[strike.ID].thaw
	l.mu.Unlock()
	if _, err := l.Stop(context.Background(), strike.ID); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, time.Second, func() bool { return !stopped(t, pid) }) {
		t.Error("frozen after stop")
	}
	if !eventually(t, 2*time.Second, func() bool { return errors.Is(syscall.Kill(thaw.Pid(), 0), syscall.ESRCH) }) {
		t.Error("the detached thaw still waits")
	}
	l.Wait()
}

func TestTheDetachedThawFiresWhenLokiDies(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	pid := sleeper(t)
	l.processes = func(string) ([]int, string) { return []int{pid}, "" }
	// Loki "dies": it froze the part and will never thaw it itself.
	if _, err := l.freeze("gone", "norn", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, time.Second, func() bool { return stopped(t, pid) }) {
		t.Fatal("not frozen")
	}
	if !eventually(t, 4*time.Second, func() bool { return !stopped(t, pid) }) {
		t.Error("the detached thaw never came")
	}
}

func TestThawAllOnShutdown(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	pid := sleeper(t)
	l.processes = func(string) ([]int, string) { return []int{pid}, "" }
	if _, err := l.freeze("a", "norn", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, time.Second, func() bool { return stopped(t, pid) }) {
		t.Fatal("not frozen")
	}
	l.ThawAll()
	if !eventually(t, time.Second, func() bool { return !stopped(t, pid) }) {
		t.Error("frozen after ThawAll")
	}
}

func TestNeverFreezesLokiOrAnUnfreezablePart(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	l.Minute = time.Minute
	var signalled []int
	l.signal = func(pid int, _ syscall.Signal) error { signalled = append(signalled, pid); return nil }
	l.processes = func(string) ([]int, string) { return []int{os.Getpid()}, "" }
	if _, err := l.Start(context.Background(), "norn", Trick{Kind: KindFreeze}, 1, "manual"); err == nil {
		t.Error("Loki froze itself")
	}
	l.processes = func(string) ([]int, string) { return nil, "Loki can't see Norn's process" }
	_, err := l.Start(context.Background(), "norn", Trick{Kind: KindFreeze}, 1, "manual")
	if refused, ok := err.(*Refused); !ok || refused.Status != http.StatusConflict || !strings.Contains(refused.Message, "can't see") {
		t.Errorf("unfreezable: %v", err)
	}
	if _, err := l.Start(context.Background(), "loki", Trick{Kind: KindFreeze}, 1, "manual"); err == nil {
		t.Error("struck Loki")
	}
	l.processes = func(string) ([]int, string) { return []int{4242}, "" }
	if _, err := l.Start(context.Background(), "ratatoskr", Trick{Kind: KindFreeze}, 1, "manual"); err == nil {
		t.Error("froze Ratatoskr without allowWatchers")
	}
	if len(signalled) != 0 {
		t.Errorf("signalled %v", signalled)
	}
}

func TestTargetsListEveryWayToStrike(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	upstream, _ := url.Parse(w.norn.URL)
	l.Proxies["norn"] = NewProxy("norn", "127.0.0.1:0", upstream, "hop")
	l.processes = func(name string) ([]int, string) {
		if name == "norn" {
			return []int{4242}, ""
		}
		return nil, "Loki can't see " + Title(name) + "'s process"
	}
	targets := l.Targets(context.Background())
	byName := map[string]Target{}
	for _, target := range targets {
		byName[target.Name] = target
	}
	norn := byName["norn"]
	if !norn.Proxied || !norn.Freezable || !norn.available(ViaSwitch) || !norn.available(ViaProxy) || !norn.available(ViaFreeze) {
		t.Errorf("norn: %+v", norn)
	}
	count := map[string]int{}
	expect := map[string]string{}
	for _, trick := range norn.Tricks {
		count[trick.Via]++
		expect[trick.Via+":"+trick.Kind] = strings.Join(trick.Expect, ",")
	}
	if count[ViaSwitch] != 3 || count[ViaProxy] != 5 || count[ViaFreeze] != 1 {
		t.Errorf("norn's tricks by via: %v", count)
	}
	for key, want := range map[string]string{
		"proxy:health":  "ratatoskr:down:norn,gjallar:alert",
		"proxy:drop":    "ratatoskr:down:norn,gjallar:alert",
		"proxy:hang":    "ratatoskr:down:norn,gjallar:alert",
		"proxy:error":   "ratatoskr:errors:norn",
		"proxy:slow":    "ratatoskr:slow:norn",
		"freeze:freeze": "ratatoskr:down:norn,gjallar:alert",
	} {
		if expect[key] != want {
			t.Errorf("%s expects %q, want %q", key, expect[key], want)
		}
	}
	hermodr := byName["hermodr"]
	for _, trick := range hermodr.Tricks {
		if trick.Via != ViaSwitch {
			t.Errorf("hermodr offers %s %s", trick.Via, trick.Kind)
		}
	}
	for _, via := range hermodr.Vias {
		if via.Via != ViaSwitch && (via.Available || via.Reason == "") {
			t.Errorf("hermodr's %s: %+v", via.Via, via)
		}
	}
	for _, via := range byName["ratatoskr"].Vias {
		if via.Available || !strings.Contains(via.Reason, "allowWatchers") {
			t.Errorf("ratatoskr's %s: %+v", via.Via, via)
		}
	}
}

func TestStrikesTakeAVia(t *testing.T) {
	for _, c := range []struct {
		trick Trick
		via   string
		ok    bool
	}{
		{Trick{Kind: "health"}, ViaSwitch, true},
		{Trick{Kind: "health", Via: ViaProxy}, ViaProxy, true},
		{Trick{Kind: KindHang}, ViaProxy, true},
		{Trick{Kind: KindFreeze}, ViaFreeze, true},
		{Trick{Kind: KindError, Status: 404}, "", false},
		{Trick{Kind: KindError, Path: "v1"}, "", false},
		{Trick{Kind: "loop", Loop: "x", Via: ViaProxy}, "", false},
		{Trick{Kind: KindDrop, Via: ViaSwitch}, "", false},
		{Trick{Kind: "health", Via: "sideways"}, "", false},
	} {
		got, err := Normalize(c.trick)
		if (err == nil) != c.ok || (c.ok && got.Via != c.via) {
			t.Errorf("%+v: %+v %v", c.trick, got, err)
		}
	}
	if got, _ := Normalize(Trick{Kind: KindError}); got.Status != 500 {
		t.Errorf("error's status: %d", got.Status)
	}
}

func TestAProxyStrikeIsCaughtFromOutside(t *testing.T) {
	w := newWorld(t)
	l := w.loki()
	upstream, _ := url.Parse(w.norn.URL)
	proxy := NewProxy("norn", "127.0.0.1:0", upstream, "hop")
	front := httptest.NewServer(proxy)
	defer front.Close()
	l.Proxies["norn"] = proxy
	// Ratatoskr and Gjallar reach Norn through the proxy, as YGG_NORN_URL would send them.
	w.seen.Store(front.URL)
	l.Minute = time.Second

	strike, err := l.Start(context.Background(), "norn", Trick{Kind: KindHang}, 1, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if strike.Trick.Via != ViaProxy {
		t.Errorf("via: %s", strike.Trick.Via)
	}
	l.Wait()
	done, _ := l.Strike(strike.ID)
	if done.State != Caught || done.HealedAt == nil {
		t.Fatalf("state %s, checks %+v", done.State, done.Checks)
	}
	if proxy.Active() != nil {
		t.Error("the proxy still tricks after the strike")
	}

	// Without a proxy in front, a proxy trick is refused.
	_, err = l.Start(context.Background(), "hermodr", Trick{Kind: KindDrop}, 1, "manual")
	if refused, ok := err.(*Refused); !ok || refused.Status != http.StatusConflict || !strings.Contains(refused.Message, "LOKI_PROXY") {
		t.Errorf("hermodr without a proxy: %v", err)
	}
}

func TestPassiveStrikesFreezeOnlyWhenAllowed(t *testing.T) {
	target := Target{Name: "norn", Up: true, LokiOn: true, Proxied: true, Freezable: true,
		Vias:   []Via{{Via: ViaSwitch, Available: true}, {Via: ViaProxy, Available: true}, {Via: ViaFreeze, Available: true}},
		Tricks: []TargetTrick{{Via: ViaSwitch, Kind: "health"}, {Via: ViaProxy, Kind: KindDrop}, {Via: ViaFreeze, Kind: KindFreeze}}}
	vias := func(tricks []TargetTrick) string {
		var list []string
		for _, trick := range tricks {
			list = append(list, trick.Via)
		}
		return strings.Join(list, ",")
	}
	if got := vias(passiveTricks(target, Settings{})); got != "switch,proxy" {
		t.Errorf("without allowFreeze: %s", got)
	}
	if got := vias(passiveTricks(target, Settings{AllowFreeze: true})); got != "switch,proxy,freeze" {
		t.Errorf("with allowFreeze: %s", got)
	}
	target.Vias[0].Available = false
	if got := vias(passiveTricks(target, Settings{})); got != "proxy" {
		t.Errorf("switch off: %s", got)
	}
	target.Up = false
	if got := passiveTricks(target, Settings{AllowFreeze: true}); len(got) != 0 {
		t.Errorf("a part that is down: %v", got)
	}
	target.Up, target.Name = true, "gjallar"
	if got := passiveTricks(target, Settings{AllowFreeze: true}); len(got) != 0 {
		t.Errorf("Gjallar: %v", got)
	}
}
