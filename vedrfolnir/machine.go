package vedrfolnir

// How hard this WSL machine is working, and who is making it. Ported from
// my-agent's runner (runner/src/system.ts).
//
// Read from /proc, so Veðrfölnir runs on the host, not in a container. A
// sample every 5 s goes into a ring kept in memory, so a spike shows up even
// when no page was open; the ring is saved every minute and on shutdown.
// Consumers are read only when asked for: walking every /proc/<pid> every
// 5 s would be its own load.
//
// Windows' own view sits beside WSL's, read through powershell.exe: how much
// of the host's memory the VM holds and how busy the host's CPU is (at most
// every 30 s), and the GPUs and Windows processes (at most every 10 s).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sampleEvery = 5 * time.Second
	historySize = 720
	// ticks is Linux's USER_HZ: /proc counts CPU time in 1/100 s.
	ticks     = 100
	pageBytes = 4096
	// powershell by full path: a systemd unit has no Windows directories on its PATH.
	powershell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
)

// Sample is one 5 s reading.
type Sample struct {
	At int64 `json:"at"`
	// CPU is percent of all cores, 0-100.
	CPU  float64 `json:"cpu"`
	Load float64 `json:"load"`
	MemoryUse
	// Pressure is /proc/pressure's `some avg10`.
	Pressure Pressure `json:"pressure"`
}

// Pressure is the share of the last 10 s some task waited, per resource.
type Pressure struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	IO     float64 `json:"io"`
}

// Disk is one mounted disk.
type Disk struct {
	Mount string `json:"mount"`
	Label string `json:"label"`
	// Side is whose disk it is: the VM's own ext4 (wsl), or a Windows drive (windows).
	Side      string  `json:"side"`
	SizeBytes float64 `json:"sizeBytes"`
	FreeBytes float64 `json:"freeBytes"`
}

// UsedPercent is how full the disk is.
func (d Disk) UsedPercent() float64 {
	if d.SizeBytes <= 0 {
		return 0
	}
	return (d.SizeBytes - d.FreeBytes) / d.SizeBytes * 100
}

// Report is GET /v1/system: the parts that answer at once.
type Report struct {
	Kernel         string     `json:"kernel"`
	Cores          int        `json:"cores"`
	UptimeSeconds  int64      `json:"uptimeSeconds"`
	MemTotalBytes  float64    `json:"memTotalBytes"`
	SwapTotalBytes float64    `json:"swapTotalBytes"`
	Load           [3]float64 `json:"load"`
	Disks          []Disk     `json:"disks"`
	History        []Sample   `json:"history"`
}

// Consumer is a group of processes: a service, or a plain process by name.
type Consumer struct {
	ServiceID string `json:"serviceId,omitempty"`
	Name      string `json:"name"`
	Processes int    `json:"processes"`
	// CPU is percent of one core, like top: 400 is four cores' worth.
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
}

// Machine holds the sampler's ring and the cached slow readings.
type Machine struct {
	HistoryPath string
	// Disks are the mounts to report: `/` is the distro's disk, `/mnt/c` Windows' C:.
	Disks []Disk

	mu      sync.Mutex
	history []Sample
	lastCPU *CPUTimes

	windows        cached[WindowsHost]
	windowsUsage   cached[*WindowsUsage]
	smi            cached[smiReading]
	scanMu         sync.Mutex
	lastScan       *scan
	lastUsage      *usageReading
	containerNames *containerReading
}

// NewMachine makes a machine that keeps its hour of samples at historyPath.
func NewMachine(historyPath string) *Machine {
	return &Machine{
		HistoryPath: historyPath,
		Disks: []Disk{
			{Mount: "/", Label: "WSL disk", Side: "wsl"},
			{Mount: "/mnt/c", Label: "C:", Side: "windows"},
		},
	}
}

func read(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	stdout, _, err := runBoth(ctx, timeout, name, args...)
	return stdout, err
}

func runBoth(ctx context.Context, timeout time.Duration, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil && stderr.Len() > 0 {
		err = errors.New(strings.SplitN(strings.TrimSpace(stderr.String()), "\n", 2)[0])
	}
	return stdout.String(), stderr.String(), err
}

// --- the sampler ------------------------------------------------------------

// Sample takes one reading into the ring. The first call only sets the clock.
func (m *Machine) Sample() {
	cpu := ParseCPU(read("/proc/stat"))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastCPU != nil {
		load, _ := strconv.ParseFloat(strings.Fields(read("/proc/loadavg") + " 0")[0], 64)
		m.history = append(m.history, Sample{
			At:        time.Now().UnixMilli(),
			CPU:       CPUPercent(*m.lastCPU, cpu),
			Load:      load,
			MemoryUse: Memory(ParseMeminfo(read("/proc/meminfo"))),
			Pressure: Pressure{
				CPU:    ParsePressure(read("/proc/pressure/cpu")),
				Memory: ParsePressure(read("/proc/pressure/memory")),
				IO:     ParsePressure(read("/proc/pressure/io")),
			},
		})
		if len(m.history) > historySize {
			m.history = m.history[len(m.history)-historySize:]
		}
	}
	m.lastCPU = &cpu
}

// SavedHistory is the samples saved in text still inside the last hour,
// oldest first; none when it is not a list of them.
func SavedHistory(text string, now int64) []Sample {
	var raw []json.RawMessage
	if json.Unmarshal([]byte(text), &raw) != nil {
		return nil
	}
	var kept []Sample
	for _, entry := range raw {
		var sample Sample
		if json.Unmarshal(entry, &sample) != nil || sample.At == 0 {
			continue
		}
		if sample.At > now-historySize*sampleEvery.Milliseconds() && sample.At <= now {
			kept = append(kept, sample)
		}
	}
	if len(kept) > historySize {
		kept = kept[len(kept)-historySize:]
	}
	return kept
}

// Load picks up the hour saved at HistoryPath.
func (m *Machine) Load() {
	saved := SavedHistory(read(m.HistoryPath), time.Now().UnixMilli())
	m.mu.Lock()
	m.history = append(saved, m.history...)
	m.mu.Unlock()
}

// Save writes the ring aside and renames it over, so a crash mid-write
// leaves the old file.
func (m *Machine) Save() error {
	m.mu.Lock()
	data, err := json.Marshal(m.history)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.HistoryPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.HistoryPath+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(m.HistoryPath+".tmp", m.HistoryPath)
}

// --- the report -------------------------------------------------------------

// ReadDisks is every configured mount that statfs can read.
func (m *Machine) ReadDisks() []Disk {
	disks := []Disk{}
	for _, disk := range m.Disks {
		var stats syscall.Statfs_t
		if syscall.Statfs(disk.Mount, &stats) != nil {
			continue
		}
		disk.SizeBytes = float64(stats.Blocks) * float64(stats.Bsize)
		disk.FreeBytes = float64(stats.Bavail) * float64(stats.Bsize)
		disks = append(disks, disk)
	}
	return disks
}

// Report is what answers at once: /proc, statfs, and the hour of samples.
func (m *Machine) Report() Report {
	meminfo := ParseMeminfo(read("/proc/meminfo"))
	report := Report{
		Kernel:         strings.TrimSpace(read("/proc/sys/kernel/osrelease")),
		Cores:          runtime.NumCPU(),
		MemTotalBytes:  meminfo["MemTotal"],
		SwapTotalBytes: meminfo["SwapTotal"],
		Disks:          m.ReadDisks(),
	}
	if uptime, err := strconv.ParseFloat(strings.Fields(read("/proc/uptime") + " 0")[0], 64); err == nil {
		report.UptimeSeconds = int64(math.Round(uptime))
	}
	loads := strings.Fields(read("/proc/loadavg"))
	for i := 0; i < 3 && i < len(loads); i++ {
		report.Load[i], _ = strconv.ParseFloat(loads[i], 64)
	}
	m.mu.Lock()
	report.History = append([]Sample{}, m.history...)
	m.mu.Unlock()
	return report
}

// --- slow readings, cached --------------------------------------------------

// cached is a slow reading refreshed behind its last answer: only a caller
// with no answer yet waits, and a failure is not retried on every poll.
type cached[T any] struct {
	mu       sync.Mutex
	value    T
	have     bool
	triedAt  time.Time
	inFlight chan struct{}
}

func (c *cached[T]) get(maxAge time.Duration, read func() (T, bool)) (T, bool) {
	c.mu.Lock()
	if c.inFlight == nil && time.Since(c.triedAt) > maxAge {
		c.triedAt = time.Now()
		done := make(chan struct{})
		c.inFlight = done
		go func() {
			value, ok := read()
			c.mu.Lock()
			if ok {
				c.value, c.have = value, true
			}
			c.triedAt = time.Now()
			c.inFlight = nil
			c.mu.Unlock()
			close(done)
		}()
	}
	waiting := c.inFlight
	have := c.have
	c.mu.Unlock()
	if !have && waiting != nil {
		<-waiting
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.have
}

var windowsScript = strings.Join([]string{
	"$vm = Get-Process vmmemWSL,vmmem -ErrorAction SilentlyContinue | Select-Object Name,WorkingSet64",
	"$os = Get-CimInstance Win32_OperatingSystem | Select-Object TotalVisibleMemorySize,FreePhysicalMemory",
	"$cpu = Get-CimInstance Win32_Processor | Select-Object LoadPercentage,NumberOfLogicalProcessors",
	"[pscustomobject]@{ vm = $vm; os = $os; cpu = $cpu } | ConvertTo-Json -Compress -Depth 3",
}, "; ")

// Windows is the host as Windows sees it; false without interop.
func (m *Machine) Windows(ctx context.Context) (WindowsHost, bool) {
	return m.windows.get(30*time.Second, func() (WindowsHost, bool) {
		out, err := run(context.WithoutCancel(ctx), 10*time.Second, powershell, "-NoProfile", "-NonInteractive", "-Command", windowsScript)
		if err != nil {
			return WindowsHost{}, false
		}
		return ParseWindows(out, time.Now().UnixMilli())
	})
}

// WindowsUsage is the host's GPUs and processes, at most every 10 s.
func (m *Machine) WindowsUsage(ctx context.Context) (*WindowsUsage, bool) {
	return m.windowsUsage.get(10*time.Second, func() (*WindowsUsage, bool) {
		out, err := run(context.WithoutCancel(ctx), 20*time.Second, powershell, "-NoProfile", "-NonInteractive", "-Command", WindowsUsageScript)
		if err != nil {
			return nil, false
		}
		return ParseWindowsUsage(out)
	})
}

type smiReading struct {
	gpu       WSLGPU
	processes []ProcessGPU
}

const noNvidia = "WSL cannot read GPU use here: Linux only gets a D3D12 passthrough, there is no nvidia-smi, and Windows did not answer."

func (m *Machine) readSMI(ctx context.Context) smiReading {
	reading, _ := m.smi.get(5*time.Second, func() (smiReading, bool) {
		ctx := context.WithoutCancel(ctx)
		out, err := run(ctx, 5*time.Second, "nvidia-smi", "--query-gpu=name,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits")
		if err != nil {
			return smiReading{gpu: WSLGPU{Source: "none", GPUs: []GPU{}, Reason: noNvidia}}, true
		}
		pmon, _ := run(ctx, 5*time.Second, "nvidia-smi", "pmon", "-c", "1", "-s", "u")
		apps, _ := run(ctx, 5*time.Second, "nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
		return smiReading{gpu: WSLGPU{Source: "nvidia-smi", GPUs: ParseNvidiaGPUs(out)}, processes: ParseNvidiaProcesses(pmon, apps)}, true
	})
	return reading
}

// GPUReport is GET /v1/system/gpu.
type GPUReport struct {
	WindowsGPUs []GPU  `json:"windowsGpus,omitempty"`
	WSLGPU      WSLGPU `json:"wslGpu"`
}

// GPU is the host's GPUs as Windows counts them, and WSL's own view.
func (m *Machine) GPU(ctx context.Context) GPUReport {
	var report GPUReport
	windows, haveWindows := m.WindowsUsage(ctx)
	report.WSLGPU = m.readSMI(ctx).gpu
	if haveWindows {
		report.WindowsGPUs = windows.GPUs
		// nvidia-smi sees each Linux process; without it, Windows' count for the VM.
		if report.WSLGPU.Source == "none" {
			if vm, ok := windows.VMGPU(); ok {
				report.WSLGPU = vm
			}
		}
	}
	return report
}

// --- consumers --------------------------------------------------------------

type seen struct {
	ProcessTimes
	rss   float64
	owner Owner
}

type scan struct {
	at        time.Time
	processes map[int]seen
}

type usageReading struct {
	at   time.Time
	list []Consumer
}

type containerReading struct {
	at    time.Time
	names map[string]Container
}

func (m *Machine) containers(ctx context.Context) map[string]Container {
	if m.containerNames != nil && time.Since(m.containerNames.at) < time.Minute {
		return m.containerNames.names
	}
	names := map[string]Container{}
	out, err := run(ctx, 5*time.Second, "docker", "ps", "--no-trunc", "--format", `{{.ID}}	{{.Names}}	{{.Label "com.docker.compose.project"}}`)
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) >= 2 && fields[0] != "" && fields[1] != "" {
				names[fields[0]] = Container{Name: fields[1], Compose: len(fields) > 2 && fields[2] != ""}
			}
		}
	}
	m.containerNames = &containerReading{at: time.Now(), names: names}
	return names
}

func scanProc(names map[string]Container, previous map[int]seen) map[int]seen {
	processes := map[int]seen{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processes
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + entry.Name()
		stat := read(dir + "/stat")
		times, ok := ParseProcessStat(stat)
		if !ok {
			continue
		}
		var rss float64
		if statm := strings.Fields(read(dir + "/statm")); len(statm) > 1 {
			pages, _ := strconv.ParseFloat(statm[1], 64)
			rss = pages * pageBytes
		}
		// A pid seen before keeps its owner: its cgroup and argv hardly change,
		// and reading them is most of the cost. Same start time, same process.
		owner := Owner{}
		if known, ok := previous[pid]; ok && known.Start == times.Start {
			owner = known.owner
		} else {
			comm := ""
			if start, end := strings.Index(stat, "("), strings.LastIndex(stat, ")"); start >= 0 && end > start {
				comm = stat[start+1 : end]
			}
			var argv []string
			for _, arg := range strings.Split(read(dir+"/cmdline"), "\x00") {
				if arg != "" {
					argv = append(argv, arg)
				}
			}
			owner = OwnerOf(read(dir+"/cgroup"), ProcessName(comm, argv), names)
		}
		processes[pid] = seen{ProcessTimes: times, rss: rss, owner: owner}
	}
	return processes
}

// Usage is every consumer by CPU since the last call, then by memory.
func (m *Machine) Usage(ctx context.Context) []Consumer {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	// Two pages polling at once would compare scans milliseconds apart, where
	// every process reads as idle: hand the last answer back instead.
	if m.lastUsage != nil && time.Since(m.lastUsage.at) < 2*time.Second {
		return m.lastUsage.list
	}
	names := m.containers(ctx)
	before := m.lastScan
	// The first call, or one long after the last, has nothing fresh to
	// compare with: take a second look a second later.
	if before == nil || time.Since(before.at) > time.Minute {
		var previous map[int]seen
		if before != nil {
			previous = before.processes
		}
		before = &scan{at: time.Now(), processes: scanProc(names, previous)}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	now := &scan{at: time.Now(), processes: scanProc(names, before.processes)}
	m.lastScan = now
	seconds := math.Max(now.at.Sub(before.at).Seconds(), 0.1)

	groups := map[string]*Consumer{}
	for pid, process := range now.processes {
		var delta float64
		if earlier, ok := before.processes[pid]; ok && earlier.Start == process.Start {
			delta = math.Max(process.Ticks-earlier.Ticks, 0)
		}
		group := groups[process.owner.Key]
		if group == nil {
			group = &Consumer{ServiceID: process.owner.ServiceID, Name: process.owner.Name}
			groups[process.owner.Key] = group
		}
		group.Processes++
		group.CPU += delta / ticks / seconds * 100
		group.MemoryBytes += process.rss
	}
	list := make([]Consumer, 0, len(groups))
	for _, group := range groups {
		group.CPU = round1(group.CPU)
		list = append(list, *group)
	}
	slices.SortFunc(list, func(a, b Consumer) int {
		if a.CPU != b.CPU {
			if b.CPU > a.CPU {
				return 1
			}
			return -1
		}
		switch {
		case b.MemoryBytes > a.MemoryBytes:
			return 1
		case b.MemoryBytes < a.MemoryBytes:
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	m.lastUsage = &usageReading{at: now.at, list: list}
	return list
}

// Consumers is the limit biggest consumers.
func (m *Machine) Consumers(ctx context.Context, limit int) []Consumer {
	list := m.Usage(ctx)
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}

// ServiceUsage is what each service with processes in WSL uses, by service id.
func (m *Machine) ServiceUsage(ctx context.Context) map[string]Consumer {
	byID := map[string]Consumer{}
	for _, consumer := range m.Usage(ctx) {
		if consumer.ServiceID != "" {
			byID[consumer.ServiceID] = consumer
		}
	}
	return byID
}

// ServiceGPU is GPU use per service id, summed over its processes, from
// nvidia-smi's pids. Empty without it, or before the first scan.
func (m *Machine) ServiceGPU(ctx context.Context) map[string]GPUUse {
	result := map[string]GPUUse{}
	m.scanMu.Lock()
	last := m.lastScan
	m.scanMu.Unlock()
	if last == nil {
		return result
	}
	for _, process := range m.readSMI(ctx).processes {
		if owner, ok := last.processes[process.PID]; ok && owner.owner.ServiceID != "" {
			sum := result[owner.owner.ServiceID]
			sum.Utilization += process.Utilization
			sum.MemoryBytes += process.MemoryBytes
			result[owner.owner.ServiceID] = sum
		}
	}
	return result
}

// ServerUsage is what a probed local server uses, from whichever side of the
// machine listens on its port.
type ServerUsage struct {
	Host      string
	Usage     *Usage
	GPU       *GPUUse
	ServiceID string
}

func portOf(rawURL string) int {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	if port, err := strconv.Atoi(parsed.Port()); err == nil {
		return port
	}
	if parsed.Scheme == "https" {
		return 443
	}
	return 80
}

// ServerUsage is what each probed server uses, by its `http:<name>` id.
// Windows first: with mirrored networking a Windows server answers on WSL's
// 127.0.0.1 too, and Windows knows its process tree and GPU use. Otherwise it
// is a WSL process, and its numbers are its service's; a container's port is
// held by root's docker-proxy, so that one goes by docker's published ports.
func (m *Machine) ServerUsage(ctx context.Context, probes []Probe, services map[string]Consumer, gpus map[string]GPUUse) map[string]ServerUsage {
	windows, haveWindows := m.WindowsUsage(ctx)
	listeningText, _ := run(ctx, 5*time.Second, "ss", "-ltnpH")
	listening, _ := ParseListening(listeningText)
	publishedText, _ := run(ctx, 5*time.Second, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	published := ParsePublished(publishedText)
	m.scanMu.Lock()
	last := m.lastScan
	m.scanMu.Unlock()

	result := map[string]ServerUsage{}
	for _, probe := range probes {
		port := portOf(probe.URL)
		if port == 0 {
			continue
		}
		id := "http:" + probe.Name
		if haveWindows {
			if pid, ok := windows.Listeners[port]; ok {
				if tree, ok := windows.Tree(pid); ok {
					server := ServerUsage{Host: "windows", Usage: &Usage{CPU: tree.CPU, MemoryBytes: tree.MemoryBytes, Processes: tree.Processes}}
					if tree.GPU.Utilization > 0 || tree.GPU.MemoryBytes > 0 {
						gpu := tree.GPU
						server.GPU = &gpu
					}
					result[id] = server
					continue
				}
			}
		}
		pid, isListening := listening[port]
		if !isListening {
			continue
		}
		server := ServerUsage{Host: "wsl"}
		if pid != 0 && last != nil {
			server.ServiceID = last.processes[pid].owner.ServiceID
		} else if name, ok := published[port]; ok {
			server.ServiceID = "docker:" + name
		}
		if server.ServiceID != "" {
			if used, ok := services[server.ServiceID]; ok {
				server.Usage = &Usage{CPU: used.CPU, MemoryBytes: used.MemoryBytes, Processes: used.Processes}
			}
			if gpu, ok := gpus[server.ServiceID]; ok {
				server.GPU = &gpu
			}
		}
		result[id] = server
	}
	return result
}
