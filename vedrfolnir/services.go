package vedrfolnir

// What runs on this machine for you, read-only: your own systemd user units,
// your docker compose containers, and the local HTTP servers the parts lean
// on. Ported from my-agent's runner (runner/src/services.ts), without its one
// action (docker stop): Veðrfölnir reports and fixes nothing.
//
//	systemd   every unit file in ~/.config/systemd/user, so a new one shows
//	          up without being listed anywhere
//	docker    every container docker compose started, grouped by project
//	http      a fixed list of local servers, up when anything answers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yggdrasil/internal/env"
)

// Service is one thing running for you.
type Service struct {
	// ID is `systemd:planlab`, `docker:my-agent-agent-1` or `http:ollama`.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Description is what it says about itself: the unit's Description, the
	// compose project, the URL.
	Description string `json:"description"`
	// State is running, stopped, failed, starting or unknown.
	State string `json:"state"`
	// Detail is the underlying words: SubState, docker's Status, the HTTP status.
	Detail string `json:"detail"`
	// Since is epoch ms it entered its current state, when known.
	Since       int64   `json:"since,omitempty"`
	MemoryBytes *int64  `json:"memoryBytes,omitempty"`
	CPUSeconds  *int64  `json:"cpuSeconds,omitempty"`
	Group       string  `json:"group,omitempty"`
	Usage       *Usage  `json:"usage,omitempty"`
	GPU         *GPUUse `json:"gpu,omitempty"`
	// Host is, for an HTTP server, which side of the machine listens on its port.
	Host string `json:"host,omitempty"`
	// ServiceID is, for an HTTP server in WSL, the unit or container it is.
	ServiceID string `json:"serviceId,omitempty"`
}

// Usage is CPU in percent of one core, resident memory, and how many processes.
type Usage struct {
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
	Processes   int     `json:"processes"`
}

// Probe is one local HTTP server to list.
type Probe struct {
	Name string
	URL  string
}

// ParseProbes reads `name=url,name=url`.
func ParseProbes(text string) []Probe {
	var probes []Probe
	for _, pair := range env.Pairs(text) {
		probes = append(probes, Probe{Name: pair.Name, URL: pair.Value})
	}
	return probes
}

// ParseShow reads `systemctl show` output, `key=value` per line.
func ParseShow(text string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && key != "" {
			fields[key] = value
		}
	}
	return fields
}

func wholeNumber(value string) (int64, bool) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil
}

// SystemdService reads one unit's `systemctl show` fields.
func SystemdService(fields map[string]string) Service {
	name := strings.TrimSuffix(fields["Id"], ".service")
	active := fields["ActiveState"]
	state := "unknown"
	switch active {
	case "active":
		state = "running"
	case "failed":
		state = "failed"
	case "activating", "reloading":
		state = "starting"
	case "inactive", "deactivating":
		state = "stopped"
	}
	stampField := fields["InactiveEnterTimestamp"]
	if state == "running" {
		stampField = fields["ActiveEnterTimestamp"]
	}
	var detail []string
	for _, part := range []string{fields["SubState"], resultIfNotSuccess(state, fields["Result"]), fields["UnitFileState"]} {
		if part != "" {
			detail = append(detail, part)
		}
	}
	service := Service{ID: "systemd:" + name, Kind: "systemd", Name: name, Description: fields["Description"], State: state, Detail: strings.Join(detail, " · ")}
	// --timestamp=unix gives "@<seconds>"; empty when it never ran.
	if stamp, ok := wholeNumber(strings.TrimPrefix(stampField, "@")); ok && stamp > 0 {
		service.Since = stamp * 1000
	}
	if memory, ok := wholeNumber(fields["MemoryCurrent"]); ok {
		service.MemoryBytes = &memory
	}
	if cpu, ok := wholeNumber(fields["CPUUsageNSec"]); ok {
		seconds := int64(float64(cpu)/1e9 + 0.5)
		service.CPUSeconds = &seconds
	}
	return service
}

func resultIfNotSuccess(state, result string) string {
	if state == "failed" || result != "success" {
		return result
	}
	return ""
}

// DockerFormat is the `docker ps --format` this reads, fields joined by tabs.
const DockerFormat = `{{.Names}}	{{.State}}	{{.Status}}	{{.Label "com.docker.compose.project"}}	{{.Label "com.docker.compose.service"}}`

// DockerService reads one line of `docker ps --format DockerFormat`.
func DockerService(line string) (Service, bool) {
	fields := strings.Split(line, "\t")
	for len(fields) < 5 {
		fields = append(fields, "")
	}
	name, state, status, project, service := fields[0], fields[1], fields[2], fields[3], fields[4]
	if name == "" {
		return Service{}, false
	}
	description := project
	if service != "" {
		description = project + " / " + service
	}
	mapped := "unknown"
	switch state {
	case "running":
		mapped = "running"
	case "restarting":
		mapped = "starting"
	case "dead":
		mapped = "failed"
	case "exited", "created":
		mapped = "stopped"
	}
	return Service{ID: "docker:" + name, Kind: "docker", Name: name, Description: description, State: mapped, Detail: status, Group: project}, true
}

// UnitNames is the unit files in your own systemd user directory; `.bak`
// and the like are not units.
func UnitNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".service") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

const showProperties = "Id,Description,ActiveState,SubState,Result,UnitFileState,ActiveEnterTimestamp,InactiveEnterTimestamp,MemoryCurrent,CPUUsageNSec"

func systemdServices(ctx context.Context, unitDir string) []Service {
	units := UnitNames(unitDir)
	services := make([]Service, len(units))
	var wg sync.WaitGroup
	for i, unit := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := run(ctx, 5*time.Second, "systemctl", "--user", "show", unit, "-p"+showProperties, "--timestamp=unix")
			if err != nil {
				name := strings.TrimSuffix(unit, ".service")
				services[i] = Service{ID: "systemd:" + name, Kind: "systemd", Name: name, State: "unknown", Detail: "systemctl did not answer"}
				return
			}
			services[i] = SystemdService(ParseShow(out))
		}()
	}
	wg.Wait()
	return services
}

func dockerServices(ctx context.Context) []Service {
	out, err := run(ctx, 5*time.Second, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project", "--format", DockerFormat)
	if err != nil {
		// No docker, or no access to it: the page shows the other two kinds.
		return nil
	}
	var services []Service
	for _, line := range strings.Split(out, "\n") {
		if service, ok := DockerService(line); ok {
			services = append(services, service)
		}
	}
	return services
}

func httpService(ctx context.Context, probe Probe) Service {
	service := Service{ID: "http:" + probe.Name, Kind: "http", Name: probe.Name, Description: probe.URL}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.URL, nil)
	if err == nil {
		var response *http.Response
		if response, err = http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
			// Any answer means it is listening: a 404 on `/` is a healthy server with no root page.
			service.State, service.Detail = "running", fmt.Sprintf("answers %d", response.StatusCode)
			return service
		}
	}
	service.State, service.Detail = "stopped", "not answering"
	return service
}

// ListServices is every unit, container and probe, in that order.
func ListServices(ctx context.Context, unitDir string, probes []Probe) []Service {
	var units, containers []Service
	probed := make([]Service, len(probes))
	var wg sync.WaitGroup
	wg.Add(2 + len(probes))
	go func() { defer wg.Done(); units = systemdServices(ctx, unitDir) }()
	go func() { defer wg.Done(); containers = dockerServices(ctx) }()
	for i, probe := range probes {
		go func() { defer wg.Done(); probed[i] = httpService(ctx, probe) }()
	}
	wg.Wait()
	services := append(append(units, containers...), probed...)
	if services == nil {
		services = []Service{}
	}
	return services
}

var serviceName = regexp.MustCompile(`^[\w.@-]+$`)

// ServiceLogs is the last lines a service logged: its journal, or the
// container's own log. An HTTP probe has none.
func ServiceLogs(ctx context.Context, id string, lines int) ([]string, error) {
	kind, name, _ := strings.Cut(id, ":")
	// Names come from the list, but this is also a URL anyone with the
	// token can type: keep them to what a unit or container name can be.
	if !serviceName.MatchString(name) {
		return nil, fmt.Errorf("not a service name: %s", name)
	}
	count := strconv.Itoa(lines)
	switch kind {
	case "systemd":
		out, err := run(ctx, 5*time.Second, "journalctl", "--user", "-u", name+".service", "-n", count, "--no-pager", "-o", "short-iso")
		if err != nil {
			return nil, err
		}
		return nonEmpty(strings.Split(out, "\n")), nil
	case "docker":
		// docker hands a container's stderr back on its own stderr. Both are
		// the log; the timestamps in front put them back in order.
		stdout, stderr, err := runBoth(ctx, 5*time.Second, "docker", "logs", "--tail", count, "--timestamps", name)
		if err != nil {
			return nil, err
		}
		all := nonEmpty(strings.Split(stdout+"\n"+stderr, "\n"))
		sort.Strings(all)
		if len(all) > lines {
			all = all[len(all)-lines:]
		}
		return all, nil
	}
	return []string{}, nil
}

func nonEmpty(lines []string) []string {
	kept := []string{}
	for _, line := range lines {
		if line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}
