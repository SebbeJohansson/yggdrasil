package sindri

// Upgrading this machine from the dashboard: `ygg upgrade <version>` started
// in a systemd user unit of its own, since the install it runs restarts
// Sindri, and a child of Sindri would be stopped with it. Its output and exit
// code are kept in ~/.local/state/yggdrasil, so the Sindri that comes back
// can say how it went.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/part"
	"yggdrasil/internal/release"
)

// upgradeStale is when an upgrade with no exit code is taken to have died.
const upgradeStale = 15 * time.Minute

// Upgrade is the last upgrade started from the dashboard.
type Upgrade struct {
	Version   string `json:"version"`
	From      string `json:"from"`
	StartedAt int64  `json:"startedAt"`
	Running   bool   `json:"running"`
	// OK and ExitCode once it has finished.
	OK       *bool  `json:"ok,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Log      string `json:"log"`
}

// Upgrader starts and reports upgrades; Dir holds their files, Run starts the command.
type Upgrader struct {
	Dir string
	// Run starts argv detached from Sindri; systemd-run by default.
	Run func(argv []string) error
	mu  sync.Mutex
}

func (u *Upgrader) path(name string) string { return filepath.Join(u.Dir, "upgrade."+name) }

// Status is the last upgrade, or nil when none was started here.
func (u *Upgrader) Status() *Upgrade {
	if u == nil {
		return nil
	}
	raw, err := os.ReadFile(u.path("json"))
	if err != nil {
		return nil
	}
	var up Upgrade
	if json.Unmarshal(raw, &up) != nil {
		return nil
	}
	if log, err := os.ReadFile(u.path("log")); err == nil {
		text := string(log)
		if len(text) > 8000 {
			text = text[len(text)-8000:]
		}
		up.Log = text
	}
	if code, err := os.ReadFile(u.path("exit")); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(code))); err == nil {
			ok := n == 0
			up.ExitCode, up.OK = &n, &ok
		}
	}
	up.Running = up.ExitCode == nil && time.Since(time.UnixMilli(up.StartedAt)) < upgradeStale
	return &up
}

// Start begins `ygg upgrade version`; refused while one runs, or on a machine built from a checkout.
func (u *Upgrader) Start(version string) (*Upgrade, error) {
	if u == nil {
		return nil, httpErr(409, "upgrading from the dashboard is off on this machine")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if version != "latest" && !release.ValidVersion(version) {
		return nil, httpErr(400, "%q is not a version like v0.3.4", version)
	}
	if !release.ValidVersion(part.Version) {
		return nil, httpErr(409, "this machine runs %s, built from a checkout: update it with git and scripts/install.sh", part.Version)
	}
	if version == part.Version {
		return nil, httpErr(409, "this machine already runs %s", version)
	}
	if last := u.Status(); last != nil && last.Running {
		return nil, httpErr(409, "an upgrade to %s is already running", last.Version)
	}
	ygg, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(u.path("exit"))
	_ = os.Remove(u.path("log"))
	up := &Upgrade{Version: version, From: part.Version, StartedAt: time.Now().UnixMilli(), Running: true}
	raw, _ := json.Marshal(up)
	if err := os.WriteFile(u.path("json"), raw, 0o600); err != nil {
		return nil, err
	}
	script := `"$0" upgrade "$1" >"$2" 2>&1; echo $? >"$3"`
	argv := []string{"sh", "-c", script, ygg, version, u.path("log"), u.path("exit")}
	if err := u.Run(argv); err != nil {
		_ = os.Remove(u.path("json"))
		return nil, fmt.Errorf("could not start the upgrade: %w", err)
	}
	return up, nil
}

// systemdRun starts argv in a transient user unit, outside Sindri's own.
func systemdRun(argv []string) error {
	args := []string{"--user", "--collect", "--quiet", "--unit", fmt.Sprintf("ygg-upgrade-%d", time.Now().Unix())}
	if home := os.Getenv("YGG_HOME"); home != "" {
		args = append(args, "--setenv=YGG_HOME="+home)
	}
	out, err := exec.Command("systemd-run", append(args, argv...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Sindri) upgradeStatus(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, map[string]any{"version": part.Version, "last": s.Upgrader.Status()})
}

func (s *Sindri) startUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Version == "" {
		part.Error(w, http.StatusBadRequest, "say which version: {\"version\": \"v0.3.4\"} or \"latest\"")
		return
	}
	answer(w)(s.Upgrader.Start(body.Version))
}
