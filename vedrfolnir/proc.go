package vedrfolnir

// Parsers: plain text in, numbers out, so the tests need no /proc. Ported
// from my-agent's runner (runner/src/system.ts), same behaviour and the same
// JSON shapes, so my-widget's system and services pages read either.

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// CPUTimes is the `cpu ` line of /proc/stat, summed.
type CPUTimes struct {
	Busy  float64
	Total float64
}

// ParseCPU reads the `cpu ` line of /proc/stat: everything but idle and
// iowait is busy.
func ParseCPU(stat string) CPUTimes {
	var line string
	for _, entry := range strings.Split(stat, "\n") {
		if strings.HasPrefix(entry, "cpu ") {
			line = entry
			break
		}
	}
	fields := strings.Fields(line)
	var values []float64
	for i := 1; i < len(fields) && i <= 8; i++ {
		value, _ := strconv.ParseFloat(fields[i], 64)
		values = append(values, value)
	}
	var total float64
	for _, value := range values {
		total += value
	}
	var idle float64
	if len(values) > 3 {
		idle += values[3]
	}
	if len(values) > 4 {
		idle += values[4]
	}
	return CPUTimes{Busy: total - idle, Total: total}
}

// round1 rounds to one decimal.
func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

// CPUPercent is the share of all cores busy between two readings, 0-100.
func CPUPercent(before, after CPUTimes) float64 {
	total := after.Total - before.Total
	if total <= 0 {
		return 0
	}
	return math.Round((after.Busy-before.Busy)/total*1000) / 10
}

var meminfoLine = regexp.MustCompile(`(?m)^(\w+(?:\(\w+\))?):\s+(\d+)( kB)?$`)

// ParseMeminfo reads /proc/meminfo in bytes, by key.
func ParseMeminfo(text string) map[string]float64 {
	fields := map[string]float64{}
	for _, match := range meminfoLine.FindAllStringSubmatch(text, -1) {
		value, _ := strconv.ParseFloat(match[2], 64)
		if match[3] != "" {
			value *= 1024
		}
		fields[match[1]] = value
	}
	return fields
}

// MemoryUse is memory split into used, cache and swap: WSL's page cache is
// memory Windows sees the VM holding, so it is kept apart from what is used.
type MemoryUse struct {
	MemUsedBytes  float64 `json:"memUsedBytes"`
	MemCacheBytes float64 `json:"memCacheBytes"`
	SwapUsedBytes float64 `json:"swapUsedBytes"`
}

// Memory splits /proc/meminfo's fields.
func Memory(fields map[string]float64) MemoryUse {
	cache := fields["Buffers"] + fields["Cached"] + fields["SReclaimable"] - fields["Shmem"]
	return MemoryUse{
		MemUsedBytes:  math.Max(fields["MemTotal"]-fields["MemAvailable"], 0),
		MemCacheBytes: math.Max(cache, 0),
		SwapUsedBytes: math.Max(fields["SwapTotal"]-fields["SwapFree"], 0),
	}
}

var pressureLine = regexp.MustCompile(`(?m)^some avg10=([\d.]+)`)

// ParsePressure is `some avg10=` from one /proc/pressure file: the share of
// the last 10 s some task waited. 0 when the kernel has none.
func ParsePressure(text string) float64 {
	match := pressureLine.FindStringSubmatch(text)
	if match == nil {
		return 0
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	return value
}

// ProcessTimes is a process's CPU ticks and its start time, which tells a
// reused pid from the same process.
type ProcessTimes struct {
	Ticks float64
	Start string
}

// ParseProcessStat reads /proc/<pid>/stat. The name sits in parentheses and
// may hold spaces, so fields count from the last `)`.
func ParseProcessStat(text string) (ProcessTimes, bool) {
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 > len(text) {
		return ProcessTimes{}, false
	}
	fields := strings.Split(text[end+2:], " ")
	// After the name: state is 0, utime 11, stime 12, starttime 19.
	if len(fields) < 13 {
		return ProcessTimes{}, false
	}
	utime, err1 := strconv.ParseFloat(fields[11], 64)
	stime, err2 := strconv.ParseFloat(fields[12], 64)
	if err1 != nil || err2 != nil {
		return ProcessTimes{}, false
	}
	start := ""
	if len(fields) > 19 {
		start = fields[19]
	}
	return ProcessTimes{Ticks: utime + stime, Start: start}, true
}

// Container is a running container by its full id.
type Container struct {
	Name string
	// Compose is whether docker compose started it; the services page lists only those.
	Compose bool
}

// Owner is who a process belongs to.
type Owner struct {
	Key string
	// ServiceID links to the services page; empty for a plain process.
	ServiceID string
	Name      string
}

var (
	dockerScope = regexp.MustCompile(`docker-([0-9a-f]{64})\.scope`)
	userUnit    = regexp.MustCompile(`user@\d+\.service/app\.slice/([\w.@-]+)\.service$`)
)

// OwnerOf finds who a process belongs to from its cgroup: a systemd unit, a
// docker container, or (outside both, as WSL's terminals and VS Code are)
// its own name.
func OwnerOf(cgroup, name string, containers map[string]Container) Owner {
	var path string
	for _, line := range strings.Split(strings.TrimSpace(cgroup), "\n") {
		if strings.HasPrefix(line, "0::") {
			path = line[3:]
			break
		}
	}
	if match := dockerScope.FindStringSubmatch(path); match != nil {
		id := match[1]
		if known, ok := containers[id]; ok {
			owner := Owner{Key: "docker:" + known.Name, Name: known.Name}
			if known.Compose {
				owner.ServiceID = owner.Key
			}
			return owner
		}
		return Owner{Key: "docker:" + id, Name: id[:12]}
	}
	if match := userUnit.FindStringSubmatch(path); match != nil {
		return Owner{Key: "systemd:" + match[1], ServiceID: "systemd:" + match[1], Name: match[1]}
	}
	return Owner{Key: "process:" + name, Name: name}
}

var (
	interpreter = regexp.MustCompile(`^(node|bun|deno|python[\d.]*)$`)
	projectOf   = regexp.MustCompile(`([^/]+)/node_modules/`)
)

// ProcessName is a readable name from argv: the program, or for an
// interpreter the script it runs. A script from a project's node_modules (a
// Nuxt dev server, vite) is named for the project, so three dev servers are
// three rows. VS Code's server runs as `node` many times over; it gets its own.
func ProcessName(comm string, argv []string) string {
	program := ""
	if len(argv) > 0 {
		program = argv[0]
	}
	if strings.Contains(program, ".vscode-server") {
		return "vscode-server"
	}
	// A daemon that rewrites its argv (`avahi-daemon: running [host]`) gets its kernel name.
	base := ""
	if !strings.ContainsAny(program, " \t:") {
		base = program[strings.LastIndex(program, "/")+1:]
	}
	if base == "" {
		base = comm
	}
	if interpreter.MatchString(base) {
		script := ""
		for _, arg := range argv[1:] {
			if !strings.HasPrefix(arg, "-") {
				script = arg
				break
			}
		}
		if match := projectOf.FindStringSubmatch(script); match != nil {
			return match[1] + " · " + base
		}
		if script != "" {
			return base + " " + script[strings.LastIndex(script, "/")+1:]
		}
	}
	return base
}

// many reads a JSON value that powershell gives as one object or a list.
func many[T any](raw json.RawMessage) []T {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []T
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one T
	if json.Unmarshal(raw, &one) == nil {
		return []T{one}
	}
	return nil
}

// WindowsHost is the host as Windows sees it.
type WindowsHost struct {
	At         int64 `json:"at"`
	TotalBytes int64 `json:"totalBytes"`
	FreeBytes  int64 `json:"freeBytes"`
	// VMBytes is vmmemWSL's working set: what Task Manager shows the WSL VM using.
	VMBytes int64 `json:"vmBytes"`
	// CPU is the whole host's load, 100 is every core. Absent when Windows did not say.
	CPU   *float64 `json:"cpu,omitempty"`
	Cores int      `json:"cores,omitempty"`
}

// ParseWindows reads powershell's JSON: the VM processes, the OS memory in
// KB, and the processors. vmmemWSL is WSL's VM; plain vmmem stands in only on
// a WSL too old to name its own. Memory is required; CPU is added when present.
func ParseWindows(text string, at int64) (WindowsHost, bool) {
	var parsed struct {
		VM json.RawMessage `json:"vm"`
		OS *struct {
			TotalVisibleMemorySize *int64 `json:"TotalVisibleMemorySize"`
			FreePhysicalMemory     *int64 `json:"FreePhysicalMemory"`
		} `json:"os"`
		CPU json.RawMessage `json:"cpu"`
	}
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return WindowsHost{}, false
	}
	type vm struct {
		Name         string `json:"Name"`
		WorkingSet64 int64  `json:"WorkingSet64"`
	}
	vms := many[vm](parsed.VM)
	var found *vm
	for _, name := range []string{"vmmemWSL", "vmmem"} {
		for i := range vms {
			if vms[i].Name == name && found == nil {
				found = &vms[i]
			}
		}
	}
	if found == nil || found.WorkingSet64 == 0 || parsed.OS == nil || parsed.OS.TotalVisibleMemorySize == nil || *parsed.OS.TotalVisibleMemorySize == 0 || parsed.OS.FreePhysicalMemory == nil {
		return WindowsHost{}, false
	}
	host := WindowsHost{
		At:         at,
		TotalBytes: *parsed.OS.TotalVisibleMemorySize * 1024,
		FreeBytes:  *parsed.OS.FreePhysicalMemory * 1024,
		VMBytes:    found.WorkingSet64,
	}
	type socket struct {
		LoadPercentage            *float64 `json:"LoadPercentage"`
		NumberOfLogicalProcessors int      `json:"NumberOfLogicalProcessors"`
	}
	var load float64
	var loaded int
	for _, s := range many[socket](parsed.CPU) {
		host.Cores += s.NumberOfLogicalProcessors
		if s.LoadPercentage != nil {
			load += *s.LoadPercentage
			loaded++
		}
	}
	if loaded > 0 {
		average := load / float64(loaded)
		host.CPU = &average
	}
	return host, true
}

var pidField = regexp.MustCompile(`pid=(\d+)`)

// ParseListening reads `ss -ltnpH`: listening port to pid, 0 when the pid is
// not ours to see (docker-proxy runs as root). The first line for a port wins.
func ParseListening(text string) (ports map[int]int, order []int) {
	ports = map[int]int{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		address := fields[3]
		port, err := strconv.Atoi(address[strings.LastIndex(address, ":")+1:])
		if err != nil || port == 0 {
			continue
		}
		pid := 0
		if match := pidField.FindStringSubmatch(line); match != nil {
			pid, _ = strconv.Atoi(match[1])
		}
		if existing, seen := ports[port]; !seen {
			ports[port] = pid
			order = append(order, port)
		} else if existing == 0 {
			ports[port] = pid
		}
	}
	return ports, order
}

var publishedPort = regexp.MustCompile(`:(\d+)->`)

// ParsePublished reads `docker ps --format '{{.Names}}\t{{.Ports}}'`:
// published host port to container name.
func ParsePublished(text string) map[int]string {
	ports := map[int]string{}
	for _, line := range strings.Split(text, "\n") {
		name, published, _ := strings.Cut(line, "\t")
		if name == "" {
			continue
		}
		for _, match := range publishedPort.FindAllStringSubmatch(published, -1) {
			port, _ := strconv.Atoi(match[1])
			ports[port] = name
		}
	}
	return ports
}
