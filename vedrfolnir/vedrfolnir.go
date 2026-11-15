// Package vedrfolnir reports on the machine: CPU, memory, disks, GPUs, and
// the services and containers running on it. Named for the hawk that sits on
// top of Yggdrasil and sees everything. It fixes nothing.
//
// It runs on the host (a systemd user unit), since only the host can see
// /proc, systemd, docker and Windows.
//
// It raises its own threshold alerts through Gjallar: a disk filling up, a
// unit that failed, a container that crashed. Not for Yggdrasil's own units
// and containers (that is Ratatoskr's), and not for servers that don't answer
// (that is Munin's). One problem, one alert.
//
// Routes (all but /health need a token). Same shapes as my-agent's runner,
// so my-widget's system and services pages read either:
//
//	GET /health
//	GET /v1/system                  CPU, memory, disks and pressure with an hour of history
//	GET /v1/system/windows          the host as Windows sees it: memory, the WSL VM, CPU
//	GET /v1/system/gpu              GPU use, from Windows and from WSL
//	GET /v1/system/consumers        the biggest consumers, grouped by service
//	GET /v1/services                systemd user units, compose containers and local HTTP servers, with their use
//	GET /v1/services/{id}/logs      ?lines= (60): the last lines one of them logged
package vedrfolnir

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/part"
	"yggdrasil/internal/watch"
)

// Name is this part's name, and its source on Gjallar.
const Name = "vedrfolnir"

// Vedrfolnir is the running reporter.
type Vedrfolnir struct {
	Machine *Machine
	UnitDir string
	Probes  []Probe
	Gjallar *alert.Client
	// OwnProject is Yggdrasil's compose project, and OwnUnitPrefix its systemd
	// units' prefix: Ratatoskr watches those, so Veðrfölnir does not alert on them.
	OwnProject    string
	OwnUnitPrefix string
	// DiskWarning and DiskCritical are how full a disk may get, in percent.
	DiskWarning  float64
	DiskCritical float64

	watch *watch.Watch
}

// New makes a reporter.
func New(machine *Machine, unitDir string, probes []Probe, gjallar *alert.Client) *Vedrfolnir {
	return &Vedrfolnir{
		Machine: machine, UnitDir: unitDir, Probes: probes, Gjallar: gjallar,
		OwnProject: "yggdrasil", OwnUnitPrefix: "ygg-",
		DiskWarning: 90, DiskCritical: 95,
		watch: watch.New(),
	}
}

// Mount adds the routes and loops to the part.
func (v *Vedrfolnir) Mount(p *part.Part) {
	p.Handle("GET /v1/system", func(w http.ResponseWriter, _ *http.Request) {
		part.JSON(w, http.StatusOK, v.Machine.Report())
	})
	p.Handle("GET /v1/system/windows", func(w http.ResponseWriter, r *http.Request) {
		host, ok := v.Machine.Windows(r.Context())
		if !ok {
			part.JSON(w, http.StatusOK, map[string]any{})
			return
		}
		part.JSON(w, http.StatusOK, map[string]any{"windows": host})
	})
	p.Handle("GET /v1/system/gpu", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, v.Machine.GPU(r.Context()))
	})
	p.Handle("GET /v1/system/consumers", func(w http.ResponseWriter, r *http.Request) {
		part.JSON(w, http.StatusOK, map[string]any{"consumers": v.Machine.Consumers(r.Context(), 12)})
	})
	p.Handle("GET /v1/services", v.services)
	p.Handle("GET /v1/services/{id}/logs", v.logs)
	p.Loop("sample", sampleEvery, func(context.Context) error {
		v.Machine.Sample()
		return nil
	})
	p.Loop("save", time.Minute, func(context.Context) error { return v.Machine.Save() })
	p.Loop("alerts", time.Minute, v.alerts)
}

func (v *Vedrfolnir) services(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	services := ListServices(ctx, v.UnitDir, v.Probes)
	usage := v.Machine.ServiceUsage(ctx)
	// After ServiceUsage: its scan is what maps nvidia-smi's and the local servers' pids to services.
	gpus := v.Machine.ServiceGPU(ctx)
	servers := v.Machine.ServerUsage(ctx, v.Probes, usage, gpus)
	for i := range services {
		service := &services[i]
		if server, ok := servers[service.ID]; ok {
			service.Host, service.Usage, service.GPU, service.ServiceID = server.Host, server.Usage, server.GPU, server.ServiceID
			continue
		}
		if used, ok := usage[service.ID]; ok {
			service.Usage = &Usage{CPU: used.CPU, MemoryBytes: used.MemoryBytes, Processes: used.Processes}
		}
		if gpu, ok := gpus[service.ID]; ok {
			service.GPU = &gpu
		}
	}
	part.JSON(w, http.StatusOK, services)
}

func (v *Vedrfolnir) logs(w http.ResponseWriter, r *http.Request) {
	lines := 60
	if text := r.URL.Query().Get("lines"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 1000 {
			part.Error(w, http.StatusBadRequest, "lines must be 1 to 1000")
			return
		}
		lines = n
	}
	logs, err := ServiceLogs(r.Context(), r.PathValue("id"), lines)
	if err != nil {
		part.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	part.JSON(w, http.StatusOK, map[string]any{"lines": logs})
}

// Checks turns what Veðrfölnir sees into checks: disks, failed units, and
// crashed containers, leaving out Yggdrasil's own.
func (v *Vedrfolnir) Checks(disks []Disk, services []Service) []watch.Result {
	var results []watch.Result
	for _, disk := range disks {
		used := disk.UsedPercent()
		result := watch.Result{Key: "disk:" + disk.Mount, OK: used < v.DiskWarning, Severity: alert.Warning,
			Title:  fmt.Sprintf("%s is %.0f%% full", disk.Label, used),
			Detail: fmt.Sprintf("%.1f GB free of %.1f GB", disk.FreeBytes/1e9, disk.SizeBytes/1e9)}
		if used >= v.DiskCritical {
			result.Severity = alert.Critical
		}
		results = append(results, result)
	}
	for _, service := range services {
		if v.own(service) || service.Kind == "http" {
			continue
		}
		results = append(results, watch.Result{
			Key: "failed:" + service.ID, OK: !Crashed(service), Severity: alert.Warning,
			Title:  fmt.Sprintf("%s %s has %s", service.Kind, service.Name, crashWord(service)),
			Detail: service.Detail,
		})
	}
	return results
}

func (v *Vedrfolnir) own(service Service) bool {
	switch service.Kind {
	case "docker":
		return service.Group == v.OwnProject
	case "systemd":
		return strings.HasPrefix(service.Name, v.OwnUnitPrefix)
	}
	return false
}

// Crashed is a service that stopped on its own: a failed unit, a dead or
// restarting container, or one that exited with a code other than 0. A
// container you stopped exits with 0 (or 143 for SIGTERM, 137 when docker
// had to kill it), and those are not alerts.
func Crashed(service Service) bool {
	switch service.Kind {
	case "systemd":
		return service.State == "failed"
	case "docker":
		if service.State == "failed" || service.State == "starting" {
			return true
		}
		code, ok := exitCode(service.Detail)
		return ok && code != 0 && code != 143 && code != 137
	}
	return false
}

func crashWord(service Service) string {
	if service.Kind == "docker" && service.State == "starting" {
		return "kept restarting"
	}
	return "crashed"
}

// exitCode reads docker's `Exited (1) 3 minutes ago`.
func exitCode(status string) (int, bool) {
	rest, found := strings.CutPrefix(status, "Exited (")
	if !found {
		return 0, false
	}
	code, _, found := strings.Cut(rest, ")")
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(code)
	return n, err == nil
}

func (v *Vedrfolnir) alerts(ctx context.Context) error {
	results := v.Checks(v.Machine.ReadDisks(), ListServices(ctx, v.UnitDir, nil))
	keys := map[string]bool{}
	for _, result := range results {
		keys[result.Key] = true
	}
	v.watch.Observe(results)
	// A unit or container that is gone is no longer failing.
	v.watch.Forget(func(key string) bool { return keys[key] })
	err := v.watch.Sync(ctx, v.Gjallar)
	if err == alert.ErrOff {
		return nil
	}
	return err
}
