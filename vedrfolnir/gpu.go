package vedrfolnir

// GPU use, from both sides of the machine, and what the local servers that
// run on Windows (Ollama, Laya, XeStrata) use. Ported from my-agent's runner
// (runner/src/gpu.ts).
//
// Windows: the GPU performance counters, read through powershell.exe. They
// exist for every vendor and count all programs, WSL's included. Utilization
// is Task Manager's: per engine type the sum over processes, then the busiest
// type. Memory is dedicated plus shared: an integrated GPU has no dedicated
// memory at all.
//
// WSL: only a D3D12 passthrough, so Linux has no GPU use of its own to read.
// Windows counts the VM's GPU work against its worker process, vmwp.exe.
// NVIDIA's nvidia-smi works in WSL and sees each Linux process, so with it
// WSL's view and per-service GPU use come from there instead.

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// GPU is one adapter's use.
type GPU struct {
	Name string `json:"name"`
	// Utilization is the busiest engine, 0-100.
	Utilization      float64 `json:"utilization"`
	MemoryUsedBytes  float64 `json:"memoryUsedBytes"`
	MemoryTotalBytes float64 `json:"memoryTotalBytes,omitempty"`
}

// WSLGPU is WSL's GPU use and where it was read.
type WSLGPU struct {
	Source string `json:"source"` // nvidia-smi, windows or none
	GPUs   []GPU  `json:"gpus"`
	Reason string `json:"reason,omitempty"`
	// VMs is, for source windows, how many VM worker processes it sums; over 1, other VMs are in it.
	VMs int `json:"vms,omitempty"`
}

// GPUUse is a process's or a service's GPU use.
type GPUUse struct {
	Utilization float64 `json:"utilization"`
	MemoryBytes float64 `json:"memoryBytes"`
}

// ProcessGPU is one Linux pid's GPU use, from nvidia-smi.
type ProcessGPU struct {
	PID int
	GPUUse
}

const mb = 1024 * 1024

// WindowsUsageScript is one powershell call for the host's GPUs, every
// process's CPU, memory and GPU, and who listens on which port. About 3.5 s.
// See the runner's gpu.ts for why it reads what it reads the way it does.
var WindowsUsageScript = strings.Join([]string{
	"$eng = 'Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine'",
	"$null = Get-CimInstance $eng",
	"$cpu = { Get-Process | ? { $_.TotalProcessorTime } | Select-Object @{n='pid';e={$_.Id}},@{n='ms';e={[math]::Round($_.TotalProcessorTime.TotalMilliseconds)}} }",
	"$before = & $cpu",
	"$clock = [Diagnostics.Stopwatch]::StartNew()",
	"Start-Sleep -Milliseconds 1000",
	"$after = & $cpu",
	"$ms = $clock.ElapsedMilliseconds",
	"$e = Get-CimInstance $eng | ? { $_.UtilizationPercentage -gt 0 } | Select-Object @{n='n';e={$_.Name}},@{n='v';e={$_.UtilizationPercentage}}",
	"$m = Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory | Select-Object @{n='n';e={$_.Name}},@{n='d';e={$_.DedicatedUsage}},@{n='s';e={$_.SharedUsage}}",
	"$pm = Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessMemory | ? { $_.DedicatedUsage -gt 0 -or $_.SharedUsage -gt 0 } | Select-Object @{n='n';e={$_.Name}},@{n='d';e={$_.DedicatedUsage}},@{n='s';e={$_.SharedUsage}}",
	"$a = Get-CimInstance Win32_VideoController | Select-Object Name",
	"$p = Get-CimInstance Win32_Process -Property Name,ProcessId,ParentProcessId,WorkingSetSize | Select-Object @{n='name';e={$_.Name}},@{n='pid';e={$_.ProcessId}},@{n='parent';e={$_.ParentProcessId}},@{n='ws';e={$_.WorkingSetSize}}",
	"$l = netstat.exe -ano -p TCP | Select-String 'LISTENING' | % { $f = -split $_.Line; [pscustomobject]@{ address = $f[1]; pid = [int]$f[4] } }",
	"[pscustomobject]@{ engines = $e; memory = $m; processMemory = $pm; adapters = $a; processes = $p; listeners = $l; cpuBefore = $before; cpuAfter = $after; ms = $ms } | ConvertTo-Json -Compress -Depth 3",
}, "; ")

type counter struct {
	N string   `json:"n"`
	V *float64 `json:"v"`
}

type memoryCounter struct {
	N string   `json:"n"`
	D *float64 `json:"d"`
	S *float64 `json:"s"`
}

func (c memoryCounter) bytes() float64 {
	var sum float64
	if c.D != nil {
		sum += *c.D
	}
	if c.S != nil {
		sum += *c.S
	}
	return sum
}

// WindowsProcess is one Windows process: CPU in percent of one core like
// top, memory its working set.
type WindowsProcess struct {
	PID         int     `json:"pid"`
	Name        string  `json:"name"`
	Parent      int     `json:"parent"`
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
	GPU         GPUUse  `json:"gpu"`
}

// WindowsUsage is what WindowsUsageScript reads.
type WindowsUsage struct {
	GPUs      []GPU
	Processes map[int]*WindowsProcess
	// Listeners maps a listening TCP port to the pid that owns it.
	Listeners map[int]int
}

// TreeUsage is a process and everything it started, summed.
type TreeUsage struct {
	CPU         float64 `json:"cpu"`
	MemoryBytes float64 `json:"memoryBytes"`
	Processes   int     `json:"processes"`
	GPU         GPUUse  `json:"gpu"`
}

var (
	luidPattern = regexp.MustCompile(`(?i)(luid_0x[0-9a-f]+_0x[0-9a-f]+)`)
	pidPattern  = regexp.MustCompile(`(?i)^pid_(\d+)_`)
	typePattern = regexp.MustCompile(`(?i)engtype_(\w*)`)
)

func luidOf(instance string) string {
	match := luidPattern.FindStringSubmatch(instance)
	if match == nil {
		return ""
	}
	return strings.ToLower(match[1])
}

func pidOf(instance string) int {
	match := pidPattern.FindStringSubmatch(instance)
	if match == nil {
		return 0
	}
	pid, _ := strconv.Atoi(match[1])
	return pid
}

func typeOf(instance string) string {
	match := typePattern.FindStringSubmatch(instance)
	if match == nil || match[1] == "" {
		return "other"
	}
	return strings.ToLower(match[1])
}

// busiest sums per key and engine type, and keeps the busiest type: that is the use.
func busiest[K comparable](engines []counter, keyOf func(string) (K, bool)) map[K]float64 {
	byKey := map[K]map[string]float64{}
	for _, engine := range engines {
		key, ok := keyOf(engine.N)
		if engine.N == "" || !ok || engine.V == nil {
			continue
		}
		if byKey[key] == nil {
			byKey[key] = map[string]float64{}
		}
		byKey[key][typeOf(engine.N)] += *engine.V
	}
	result := map[K]float64{}
	for key, types := range byKey {
		var top float64
		for _, value := range types {
			top = math.Max(top, value)
		}
		result[key] = math.Min(round1(top), 100)
	}
	return result
}

func windowsGPUs(engines []counter, memoryCounters []memoryCounter, adapters []struct {
	Name string `json:"Name"`
}) []GPU {
	utilization := busiest(engines, func(instance string) (string, bool) {
		luid := luidOf(instance)
		return luid, luid != ""
	})
	memory := map[string]float64{}
	for _, c := range memoryCounters {
		if luid := luidOf(c.N); luid != "" {
			memory[luid] = c.bytes()
		}
	}
	seen := map[string]bool{}
	var luids []string
	for luid := range utilization {
		if !seen[luid] {
			seen[luid] = true
			luids = append(luids, luid)
		}
	}
	for luid := range memory {
		if !seen[luid] {
			seen[luid] = true
			luids = append(luids, luid)
		}
	}
	sort.Strings(luids)
	var names []string
	for _, adapter := range adapters {
		if adapter.Name != "" {
			names = append(names, adapter.Name)
		}
	}
	var gpus []GPU
	if len(names) == 1 {
		// One video controller still shows several luids (the same GPU's other
		// engines, a software renderer): fold them into the one name.
		folded := GPU{Name: names[0]}
		for _, luid := range luids {
			folded.Utilization = math.Max(folded.Utilization, utilization[luid])
			folded.MemoryUsedBytes += math.Round(memory[luid])
		}
		gpus = []GPU{folded}
	} else {
		for i, luid := range luids {
			name := "GPU " + strconv.Itoa(i+1)
			if len(names) == len(luids) {
				name = names[i]
			}
			gpus = append(gpus, GPU{Name: name, Utilization: utilization[luid], MemoryUsedBytes: math.Round(memory[luid])})
		}
	}
	busy := []GPU{}
	for _, gpu := range gpus {
		if gpu.Utilization > 0 || gpu.MemoryUsedBytes > 0 {
			busy = append(busy, gpu)
		}
	}
	return busy
}

// ParseWindowsUsage reads WindowsUsageScript's JSON.
func ParseWindowsUsage(text string) (*WindowsUsage, bool) {
	var parsed struct {
		Engines       json.RawMessage `json:"engines"`
		Memory        json.RawMessage `json:"memory"`
		ProcessMemory json.RawMessage `json:"processMemory"`
		Adapters      json.RawMessage `json:"adapters"`
		Processes     json.RawMessage `json:"processes"`
		Listeners     json.RawMessage `json:"listeners"`
		CPUBefore     json.RawMessage `json:"cpuBefore"`
		CPUAfter      json.RawMessage `json:"cpuAfter"`
		MS            *float64        `json:"ms"`
	}
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return nil, false
	}
	engines := many[counter](parsed.Engines)
	gpuByPID := busiest(engines, func(instance string) (int, bool) {
		pid := pidOf(instance)
		return pid, pid != 0
	})
	gpuMemoryByPID := map[int]float64{}
	for _, c := range many[memoryCounter](parsed.ProcessMemory) {
		if pid := pidOf(c.N); pid != 0 {
			gpuMemoryByPID[pid] += c.bytes()
		}
	}
	type cpuTime struct {
		PID int      `json:"pid"`
		MS  *float64 `json:"ms"`
	}
	before := map[int]float64{}
	for _, entry := range many[cpuTime](parsed.CPUBefore) {
		if entry.PID != 0 && entry.MS != nil {
			before[entry.PID] = *entry.MS
		}
	}
	elapsed := 1000.0
	if parsed.MS != nil {
		elapsed = *parsed.MS
	}
	elapsed = math.Max(elapsed, 1)
	cpuByPID := map[int]float64{}
	for _, entry := range many[cpuTime](parsed.CPUAfter) {
		if was, ok := before[entry.PID]; ok && entry.PID != 0 && entry.MS != nil {
			cpuByPID[entry.PID] = math.Max(*entry.MS-was, 0) / elapsed * 100
		}
	}
	usage := &WindowsUsage{Processes: map[int]*WindowsProcess{}, Listeners: map[int]int{}}
	type process struct {
		Name   string  `json:"name"`
		PID    int     `json:"pid"`
		Parent int     `json:"parent"`
		WS     float64 `json:"ws"`
	}
	for _, p := range many[process](parsed.Processes) {
		if p.PID == 0 {
			continue
		}
		usage.Processes[p.PID] = &WindowsProcess{
			PID: p.PID, Name: p.Name, Parent: p.Parent, CPU: cpuByPID[p.PID], MemoryBytes: p.WS,
			GPU: GPUUse{Utilization: gpuByPID[p.PID], MemoryBytes: gpuMemoryByPID[p.PID]},
		}
	}
	// 127.0.0.1:11434, 0.0.0.0:8000, [::]:8095. Pid 4 is http.sys, which
	// listens for other programs; a server's own process is what counts.
	type listener struct {
		Address string `json:"address"`
		PID     int    `json:"pid"`
	}
	for _, l := range many[listener](parsed.Listeners) {
		port, _ := strconv.Atoi(l.Address[strings.LastIndex(l.Address, ":")+1:])
		if _, taken := usage.Listeners[port]; port != 0 && l.PID > 4 && !taken {
			usage.Listeners[port] = l.PID
		}
	}
	usage.GPUs = windowsGPUs(engines, many[memoryCounter](parsed.Memory), many[struct {
		Name string `json:"Name"`
	}](parsed.Adapters))
	return usage, true
}

// Tree sums root and every process it started: Ollama serves from one
// process and runs each model in a child, which is where the GPU work is. The
// walk goes by parent pid and stops at a cycle.
func (u *WindowsUsage) Tree(root int) (TreeUsage, bool) {
	if _, ok := u.Processes[root]; !ok {
		return TreeUsage{}, false
	}
	children := map[int][]int{}
	pids := make([]int, 0, len(u.Processes))
	for pid := range u.Processes {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	for _, pid := range pids {
		p := u.Processes[pid]
		children[p.Parent] = append(children[p.Parent], pid)
	}
	var sum TreeUsage
	seen := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		p, ok := u.Processes[pid]
		if seen[pid] || !ok {
			continue
		}
		seen[pid] = true
		sum.CPU += p.CPU
		sum.MemoryBytes += p.MemoryBytes
		sum.Processes++
		sum.GPU.Utilization += p.GPU.Utilization
		sum.GPU.MemoryBytes += p.GPU.MemoryBytes
		queue = append(queue, children[pid]...)
	}
	sum.CPU = round1(sum.CPU)
	sum.GPU.Utilization = math.Min(round1(sum.GPU.Utilization), 100)
	return sum, true
}

// VMGPU is WSL's GPU use as Windows counts it: the VM worker processes
// (vmwp.exe), summed. False when no VM worker runs.
func (u *WindowsUsage) VMGPU() (WSLGPU, bool) {
	var utilization, memory float64
	workers := 0
	for _, p := range u.Processes {
		if strings.EqualFold(p.Name, "vmwp.exe") {
			workers++
			utilization += p.GPU.Utilization
			memory += p.GPU.MemoryBytes
		}
	}
	if workers == 0 {
		return WSLGPU{}, false
	}
	name := "GPU"
	if len(u.GPUs) > 0 {
		name = u.GPUs[0].Name
	}
	return WSLGPU{Source: "windows", GPUs: []GPU{{Name: name, Utilization: math.Min(round1(utilization), 100), MemoryUsedBytes: memory}}, VMs: workers}, true
}

// ParseNvidiaGPUs reads `--query-gpu=name,utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits`.
func ParseNvidiaGPUs(text string) []GPU {
	gpus := []GPU{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		utilization, err1 := strconv.ParseFloat(fields[1], 64)
		used, err2 := strconv.ParseFloat(fields[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		gpu := GPU{Name: fields[0], Utilization: utilization, MemoryUsedBytes: used * mb}
		if len(fields) > 3 {
			if total, err := strconv.ParseFloat(fields[3], 64); err == nil {
				gpu.MemoryTotalBytes = total * mb
			}
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

// ParseNvidiaProcesses reads `nvidia-smi pmon -c 1 -s u` (sm % per pid, `-`
// when idle) and `--query-compute-apps=pid,used_memory`. A process in either
// list is reported, in the order first seen.
func ParseNvidiaProcesses(pmon, apps string) []ProcessGPU {
	byPID := map[int]*ProcessGPU{}
	var order []int
	entry := func(pid int) *ProcessGPU {
		if existing, ok := byPID[pid]; ok {
			return existing
		}
		created := &ProcessGPU{PID: pid}
		byPID[pid] = created
		order = append(order, pid)
		return created
	}
	for _, line := range strings.Split(pmon, "\n") {
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if strings.HasPrefix(trimmed, "#") || len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			continue
		}
		p := entry(pid)
		if len(fields) > 3 {
			if sm, err := strconv.ParseFloat(fields[3], 64); err == nil {
				p.Utilization = math.Max(p.Utilization, sm)
			}
		}
	}
	for _, line := range strings.Split(apps, "\n") {
		pidText, usedText, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(pidText))
		used, err2 := strconv.ParseFloat(strings.TrimSpace(usedText), 64)
		if err1 != nil || err2 != nil || pid == 0 {
			continue
		}
		entry(pid).MemoryBytes += used * mb
	}
	result := make([]ProcessGPU, 0, len(order))
	for _, pid := range order {
		result = append(result, *byPID[pid])
	}
	return result
}
