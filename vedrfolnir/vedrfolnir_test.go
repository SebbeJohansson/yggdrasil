package vedrfolnir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCPUUseBetweenTwoReadings(t *testing.T) {
	before := ParseCPU("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4")
	after := ParseCPU("cpu  200 0 200 1500 100 0 0 0 0 0\n")
	if before != (CPUTimes{Busy: 200, Total: 1000}) {
		t.Fatalf("got %+v", before)
	}
	if got := CPUPercent(before, after); got != 20 {
		t.Fatalf("got %v, want 20", got)
	}
	if got := CPUPercent(after, after); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestMemoryIsSplitIntoUsedCacheAndSwap(t *testing.T) {
	fields := ParseMeminfo(strings.Join([]string{
		"MemTotal:       32612240 kB",
		"MemAvailable:   12729208 kB",
		"Buffers:          864964 kB",
		"Cached:          8786024 kB",
		"SReclaimable:     500000 kB",
		"Shmem:            100000 kB",
		"Active(anon):     123 kB",
		"SwapTotal:       8388608 kB",
		"SwapFree:        8388236 kB",
		"HugePages_Total:       0",
	}, "\n"))
	if fields["Active(anon)"] != 123*1024 || fields["HugePages_Total"] != 0 {
		t.Fatalf("got %v", fields)
	}
	want := MemoryUse{
		MemUsedBytes:  (32612240 - 12729208) * 1024,
		MemCacheBytes: (864964 + 8786024 + 500000 - 100000) * 1024,
		SwapUsedBytes: 372 * 1024,
	}
	if got := Memory(fields); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPressure(t *testing.T) {
	if got := ParsePressure("some avg10=12.50 avg60=0.01 avg300=0.00 total=57025168\nfull avg10=1.00 avg60=0.00 avg300=0.00 total=0"); got != 12.5 {
		t.Fatalf("got %v", got)
	}
	if got := ParsePressure(""); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestProcessTicksPastANameWithSpacesAndParentheses(t *testing.T) {
	got, ok := ParseProcessStat("4242 (Web (Content) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 1500 250 0 0 20 0 30 0 987654 1000 200")
	if !ok || got != (ProcessTimes{Ticks: 1750, Start: "987654"}) {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

func TestOwnerOf(t *testing.T) {
	id := "df7c441ee1687b5f1180285c9d7912e90faa2ead9d6d8626f1ffde17e3e1f77c"
	containers := map[string]Container{id: {Name: "my-agent-agent-1", Compose: true}, strings.Repeat("b", 64): {Name: "affectionate_borg"}}
	cases := []struct {
		cgroup, name string
		want         Owner
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/planlab.service\n", "python3", Owner{Key: "systemd:planlab", ServiceID: "systemd:planlab", Name: "planlab"}},
		{"0::/system.slice/docker-" + id + ".scope", "node", Owner{Key: "docker:my-agent-agent-1", ServiceID: "docker:my-agent-agent-1", Name: "my-agent-agent-1"}},
		{"0::/system.slice/docker-" + strings.Repeat("a", 64) + ".scope", "node", Owner{Key: "docker:" + strings.Repeat("a", 64), Name: "aaaaaaaaaaaa"}},
		// Not from compose: no link, since the services page does not list it.
		{"0::/system.slice/docker-" + strings.Repeat("b", 64) + ".scope", "node", Owner{Key: "docker:affectionate_borg", Name: "affectionate_borg"}},
		{"0::/non-systemd", "claude", Owner{Key: "process:claude", Name: "claude"}},
	}
	for _, c := range cases {
		if got := OwnerOf(c.cgroup, c.name, containers); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.cgroup, got, c.want)
		}
	}
}

func TestProcessName(t *testing.T) {
	cases := []struct {
		comm string
		argv []string
		want string
	}{
		{"MainThread", []string{"/home/me/.vscode-server-insiders/bin/abc/node", "/home/me/x.js"}, "vscode-server"},
		{"node", []string{"node", "--watch", "src/server.ts"}, "node server.ts"},
		{"node", []string{"/usr/bin/node", "--enable-source-maps", "/home/me/projects/my-widget/node_modules/@nuxt/cli/dist/dev/index.mjs"}, "my-widget · node"},
		{"python3", []string{"/usr/bin/python3", "./planlab.py", "--farm"}, "python3 planlab.py"},
		{"claude", []string{"/home/me/.local/bin/claude", "--resume"}, "claude"},
		{"kworker/0:1", nil, "kworker/0:1"},
		{"avahi-daemon", []string{"avahi-daemon: running [laptop.local]"}, "avahi-daemon"},
	}
	for _, c := range cases {
		if got := ProcessName(c.comm, c.argv); got != c.want {
			t.Errorf("%v: got %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestWindowsMemoryPrefersVmmemWSL(t *testing.T) {
	both := `{"vm":[{"Name":"vmmem","WorkingSet64":211705856},{"Name":"vmmemWSL","WorkingSet64":18418040832}],"os":{"TotalVisibleMemorySize":66592948,"FreePhysicalMemory":4169960}}`
	got, ok := ParseWindows(both, 1)
	if !ok || got.TotalBytes != 66592948*1024 || got.FreeBytes != 4169960*1024 || got.VMBytes != 18418040832 || got.CPU != nil {
		t.Fatalf("got %+v", got)
	}
	// One process comes back as an object, not a list.
	if got, ok := ParseWindows(`{"vm":{"Name":"vmmem","WorkingSet64":1000},"os":{"TotalVisibleMemorySize":10,"FreePhysicalMemory":5}}`, 1); !ok || got.VMBytes != 1000 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := ParseWindows(`{"vm":null,"os":{"TotalVisibleMemorySize":10,"FreePhysicalMemory":5}}`, 1); ok {
		t.Fatalf("no VM parsed as a host")
	}
	if _, ok := ParseWindows("not json", 1); ok {
		t.Fatalf("not json parsed")
	}
}

func TestWindowsCPUIsAveragedOverSockets(t *testing.T) {
	memory := `"vm":{"Name":"vmmemWSL","WorkingSet64":1000},"os":{"TotalVisibleMemorySize":10,"FreePhysicalMemory":5}`
	one, _ := ParseWindows(`{`+memory+`,"cpu":{"LoadPercentage":35,"NumberOfLogicalProcessors":16}}`, 1)
	if one.CPU == nil || *one.CPU != 35 || one.Cores != 16 {
		t.Fatalf("got %+v", one)
	}
	two, _ := ParseWindows(`{`+memory+`,"cpu":[{"LoadPercentage":20,"NumberOfLogicalProcessors":8},{"LoadPercentage":40,"NumberOfLogicalProcessors":8}]}`, 1)
	if two.CPU == nil || *two.CPU != 30 || two.Cores != 16 {
		t.Fatalf("got %+v", two)
	}
	none, _ := ParseWindows(`{`+memory+`,"cpu":{"LoadPercentage":null}}`, 1)
	if none.VMBytes != 1000 || none.CPU != nil {
		t.Fatalf("got %+v", none)
	}
}

func TestListeningAndPublishedPorts(t *testing.T) {
	ports, order := ParseListening("LISTEN 0 511 127.0.0.1:8788 0.0.0.0:* users:((\"node-MainThread\",pid=776079,fd=21))\nLISTEN 0 4096 127.0.0.1:8010 0.0.0.0:*\nLISTEN 0 4096 [::1]:8788 [::]:*\n")
	if !reflect.DeepEqual(order, []int{8788, 8010}) || ports[8788] != 776079 || ports[8010] != 0 {
		t.Fatalf("got %v %v", ports, order)
	}
	if got := ParsePublished("my-agent-languagetool-1\t127.0.0.1:8010->8010/tcp\nshell-mcp\t\n"); !reflect.DeepEqual(got, map[int]string{8010: "my-agent-languagetool-1"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSavedHistoryKeepsTheLastHourOnly(t *testing.T) {
	const now = 10_000_000
	text := `[{"at":6000000,"cpu":1},{"at":9940000,"cpu":5},{"at":10000001,"cpu":9},null]`
	got := SavedHistory(text, now)
	if len(got) != 1 || got[0].At != 9940000 || got[0].CPU != 5 {
		t.Fatalf("got %+v", got)
	}
	if got := SavedHistory(`{"not":"a list"}`, now); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if got := SavedHistory("", now); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestHistorySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	m := NewMachine(path)
	m.Sample()
	m.Sample()
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	again := NewMachine(path)
	again.Load()
	if len(again.Report().History) != 1 {
		t.Fatalf("got %d samples back", len(again.Report().History))
	}
}

const luid = "_luid_0x00000000_0x00013e05_phys_0"

func gpusOf(t *testing.T, value any) []GPU {
	t.Helper()
	payload, _ := json.Marshal(value)
	usage, ok := ParseWindowsUsage(string(payload))
	if !ok {
		t.Fatalf("did not parse %s", payload)
	}
	return usage.GPUs
}

func TestWindowsGPUIsTheBusiestEngineTypeAndDedicatedPlusShared(t *testing.T) {
	got := gpusOf(t, map[string]any{
		"engines": []map[string]any{
			{"n": "pid_10" + luid + "_eng_0_engtype_3D", "v": 30},
			{"n": "pid_11" + luid + "_eng_0_engtype_3D", "v": 25},
			{"n": "pid_10" + luid + "_eng_1_engtype_Copy", "v": 60.04},
			{"n": "garbage", "v": 99},
		},
		"memory":   []map[string]any{{"n": "luid_0x00000000_0x00013E05_phys_0", "d": 1_000_000, "s": 500}},
		"adapters": map[string]any{"Name": "Intel(R) Arc(TM) B390 GPU"},
	})
	want := []GPU{{Name: "Intel(R) Arc(TM) B390 GPU", Utilization: 60, MemoryUsedBytes: 1_000_500}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAdaptersAreNumberedWhenTheListDoesNotMatch(t *testing.T) {
	other := "_luid_0x00000000_0x00014278_phys_0"
	got := gpusOf(t, map[string]any{
		"engines":  []map[string]any{{"n": "pid_1" + luid + "_engtype_3D", "v": 5}, {"n": "pid_1" + other + "_engtype_3D", "v": 0}},
		"memory":   []map[string]any{{"n": "luid_0x00000000_0x00014278_phys_0", "d": 0, "s": 0}},
		"adapters": []map[string]any{{"Name": "a"}, {"Name": "b"}, {"Name": "c"}},
	})
	if !reflect.DeepEqual(got, []GPU{{Name: "GPU 1", Utilization: 5}}) {
		t.Fatalf("got %+v", got)
	}
	folded := gpusOf(t, map[string]any{
		"engines":  []map[string]any{{"n": "pid_1" + luid + "_engtype_3D", "v": 5}, {"n": "pid_1" + other + "_engtype_3D", "v": 8}},
		"memory":   []any{},
		"adapters": map[string]any{"Name": "Arc"},
	})
	if !reflect.DeepEqual(folded, []GPU{{Name: "Arc", Utilization: 8}}) {
		t.Fatalf("got %+v", folded)
	}
	if _, ok := ParseWindowsUsage("not json"); ok {
		t.Fatalf("not json parsed")
	}
	if got := gpusOf(t, map[string]any{"engines": nil, "memory": nil}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestWindowsProcessesAndTheirTrees(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"engines":       []map[string]any{{"n": "pid_20" + luid + "_eng_0_engtype_Compute", "v": 40}, {"n": "pid_20" + luid + "_eng_1_engtype_Compute", "v": 10}},
		"processMemory": []map[string]any{{"n": "pid_20" + luid, "d": 0, "s": 2000}},
		"processes":     []map[string]any{{"pid": 10, "parent": 1, "ws": 100}, {"pid": 20, "parent": 10, "ws": 300}, {"pid": 30, "parent": 2, "ws": 50}},
		"cpuBefore":     []map[string]any{{"pid": 10, "ms": 1000}, {"pid": 20, "ms": 5000}},
		"cpuAfter":      []map[string]any{{"pid": 10, "ms": 1100}, {"pid": 20, "ms": 6000}},
		"ms":            1000,
		"listeners":     []map[string]any{{"address": "127.0.0.1:11434", "pid": 10}, {"address": "0.0.0.0:445", "pid": 4}, {"address": "[::]:8000", "pid": 30}},
	})
	usage, ok := ParseWindowsUsage(string(payload))
	if !ok {
		t.Fatal("did not parse")
	}
	want := WindowsProcess{PID: 20, Parent: 10, CPU: 100, MemoryBytes: 300, GPU: GPUUse{Utilization: 50, MemoryBytes: 2000}}
	if *usage.Processes[20] != want {
		t.Fatalf("got %+v", *usage.Processes[20])
	}
	if !reflect.DeepEqual(usage.Listeners, map[int]int{11434: 10, 8000: 30}) {
		t.Fatalf("listeners %v", usage.Listeners)
	}
	// Ollama and the model runner it started, summed.
	tree, ok := usage.Tree(10)
	if !ok || tree != (TreeUsage{CPU: 110, MemoryBytes: 400, Processes: 2, GPU: GPUUse{Utilization: 50, MemoryBytes: 2000}}) {
		t.Fatalf("got %+v", tree)
	}
	if _, ok := usage.Tree(99); ok {
		t.Fatalf("a missing root has a tree")
	}
}

func TestWSLGPUComesFromTheVMWorker(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"engines":       []map[string]any{{"n": "pid_6488" + luid + "_eng_0_engtype_3D", "v": 15}, {"n": "pid_7556" + luid + "_eng_0_engtype_3D", "v": 40}},
		"processMemory": []map[string]any{{"n": "pid_6488" + luid, "d": 0, "s": 3000}},
		"adapters":      map[string]any{"Name": "Arc"},
		"processes":     []map[string]any{{"name": "vmwp.exe", "pid": 6488, "parent": 1}, {"name": "vmmemWSL", "pid": 7556, "parent": 1}},
	})
	usage, _ := ParseWindowsUsage(string(payload))
	got, ok := usage.VMGPU()
	want := WSLGPU{Source: "windows", GPUs: []GPU{{Name: "Arc", Utilization: 15, MemoryUsedBytes: 3000}}, VMs: 1}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	none, _ := ParseWindowsUsage(`{"processes":[{"name":"dwm.exe","pid":1}]}`)
	if _, ok := none.VMGPU(); ok {
		t.Fatalf("a VM GPU without a VM worker")
	}
}

func TestATreeStopsAtACycle(t *testing.T) {
	usage, _ := ParseWindowsUsage(`{"processes":[{"pid":1,"parent":2,"ws":1},{"pid":2,"parent":1,"ws":1}]}`)
	if tree, _ := usage.Tree(1); tree.Processes != 2 {
		t.Fatalf("got %+v", tree)
	}
}

func TestNvidiaSMI(t *testing.T) {
	got := ParseNvidiaGPUs("NVIDIA GeForce RTX 4070, 12, 1024, 12282\n")
	want := []GPU{{Name: "NVIDIA GeForce RTX 4070", Utilization: 12, MemoryUsedBytes: 1024 * mb, MemoryTotalBytes: 12282 * mb}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got := ParseNvidiaGPUs("No devices were found"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	pmon := "# gpu         pid   type     sm    mem    enc    dec    jpg    ofa    command\n# Idx           #    C/G      %      %      %      %      %      %    name\n    0       42     C     35     10      -      -      -      -    python\n    0       43     C      -      -      -      -      -      -    ollama"
	processes := ParseNvidiaProcesses(pmon, "42, 2048\n44, 100\n")
	wantProcesses := []ProcessGPU{
		{PID: 42, GPUUse: GPUUse{Utilization: 35, MemoryBytes: 2048 * mb}},
		{PID: 43},
		{PID: 44, GPUUse: GPUUse{MemoryBytes: 100 * mb}},
	}
	if !reflect.DeepEqual(processes, wantProcesses) {
		t.Fatalf("got %+v", processes)
	}
}

func TestSystemdUnits(t *testing.T) {
	running := SystemdService(ParseShow(strings.Join([]string{
		"Id=my-agent-runner.service", "Description=my-agent runner", "ActiveState=active", "SubState=running",
		"Result=success", "UnitFileState=enabled", "ActiveEnterTimestamp=@1793005880", "MemoryCurrent=265715712", "CPUUsageNSec=72476596000",
	}, "\n")))
	if running.ID != "systemd:my-agent-runner" || running.State != "running" || running.Detail != "running · enabled" ||
		running.Since != 1793005880000 || *running.MemoryBytes != 265715712 || *running.CPUSeconds != 72 {
		t.Fatalf("got %+v", running)
	}
	never := SystemdService(ParseShow("Id=planlab.service\nActiveState=inactive\nSubState=dead\nResult=success\nUnitFileState=disabled\nActiveEnterTimestamp=\nMemoryCurrent=[not set]\nCPUUsageNSec=[not set]"))
	if never.State != "stopped" || never.Since != 0 || never.MemoryBytes != nil {
		t.Fatalf("got %+v", never)
	}
	failed := SystemdService(ParseShow("Id=x.service\nActiveState=failed\nSubState=failed\nResult=exit-code"))
	if failed.State != "failed" || !strings.Contains(failed.Detail, "exit-code") {
		t.Fatalf("got %+v", failed)
	}
}

func TestComposeContainers(t *testing.T) {
	got, _ := DockerService("my-agent-agent-1\trunning\tUp 2 hours (healthy)\tmy-agent\tagent")
	want := Service{ID: "docker:my-agent-agent-1", Kind: "docker", Name: "my-agent-agent-1", Description: "my-agent / agent", State: "running", Detail: "Up 2 hours (healthy)", Group: "my-agent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if exited, _ := DockerService("hereandnow-mssql\texited\tExited (255) 7 days ago\tapp-hr-hereandnow\tmssql"); exited.State != "stopped" {
		t.Fatalf("got %+v", exited)
	}
}

func TestProbesAndUnitFiles(t *testing.T) {
	got := ParseProbes("ollama=http://127.0.0.1:11434, bad ,my-agent=http://127.0.0.1:8787/health")
	if !reflect.DeepEqual(got, []Probe{{Name: "ollama", URL: "http://127.0.0.1:11434"}, {Name: "my-agent", URL: "http://127.0.0.1:8787/health"}}) {
		t.Fatalf("got %+v", got)
	}
	dir := t.TempDir()
	for _, file := range []string{"planlab.service", "planlab.service.bak", "scarecrow.timer", "autoship.service"} {
		os.WriteFile(filepath.Join(dir, file), nil, 0o600)
	}
	if units := UnitNames(dir); !reflect.DeepEqual(units, []string{"autoship.service", "planlab.service"}) {
		t.Fatalf("got %v", units)
	}
}

func TestLogsRefuseANameNoUnitCouldHave(t *testing.T) {
	if _, err := ServiceLogs(t.Context(), "systemd:../../etc/passwd", 10); err == nil {
		t.Fatalf("a path was accepted as a unit name")
	}
}

func TestChecksLeaveOutYggdrasilsOwnAndStoppedContainers(t *testing.T) {
	v := New(NewMachine(""), "", nil, nil)
	results := v.Checks(
		[]Disk{{Mount: "/", Label: "WSL disk", SizeBytes: 100, FreeBytes: 4}},
		[]Service{
			{ID: "docker:yggdrasil-gjallar-1", Kind: "docker", Name: "yggdrasil-gjallar-1", State: "failed", Group: "yggdrasil"},
			{ID: "systemd:ygg-ratatoskr", Kind: "systemd", Name: "ygg-ratatoskr", State: "failed"},
			{ID: "systemd:planlab", Kind: "systemd", Name: "planlab", State: "failed"},
			{ID: "docker:db", Kind: "docker", Name: "db", State: "stopped", Detail: "Exited (0) 2 days ago", Group: "app"},
			{ID: "docker:api", Kind: "docker", Name: "api", State: "stopped", Detail: "Exited (1) 2 minutes ago", Group: "app"},
			{ID: "docker:web", Kind: "docker", Name: "web", State: "stopped", Detail: "Exited (143) 1 hour ago", Group: "app"},
			{ID: "http:ollama", Kind: "http", Name: "ollama", State: "stopped"},
		})
	got := map[string]bool{}
	for _, result := range results {
		got[result.Key] = result.OK
		if result.Key == "disk:/" && result.Severity != "critical" {
			t.Errorf("a disk 96%% full is %s, want critical", result.Severity)
		}
	}
	want := map[string]bool{"disk:/": false, "failed:systemd:planlab": false, "failed:docker:db": true, "failed:docker:api": false, "failed:docker:web": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
