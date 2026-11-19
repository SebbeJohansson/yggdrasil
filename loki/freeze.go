package loki

// Freeze: Loki stops a part's process with SIGSTOP and lets it go with
// SIGCONT at the strike's until. The part answers nothing while it is
// frozen: its port still takes connections, but nothing reads them, as a
// part stuck in a deadlock would.
//
// The thaw must happen even if Loki dies, so it is scheduled when the strike
// starts in a process of its own, in a session of its own, that outlives
// Loki: `sh -c 'sleep N; kill -CONT <pid>'`. Loki also thaws when the strike
// ends or is stopped, and when Loki shuts down; the detached thaw is then
// called off, so it can't cut a later freeze short.
//
// Loki can signal only the processes in its own PID namespace that run as
// its own user. A part is freezable when exactly one such process runs as
// `ygg <part>`: in dev mode, Loki in the dev container sees Norn, Gjallar,
// Munin, Huginn, Dellingr and Hermóðr, but not Sindri, Veðrfölnir and
// Ratatoskr, which run on the host (ygg-dev.service).

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FindProcesses is the process running as `ygg <name>` that Loki may signal,
// read from procDir (/proc), or, when there is none, why the part can't be
// frozen. Never Loki itself.
func FindProcesses(procDir, name string) ([]int, string) {
	if name == Name {
		return nil, "Loki never freezes itself"
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Sprintf("Loki can't read %s here, so it can't find %s's process", procDir, Title(name))
	}
	self := os.Getpid()
	var mine []int
	others := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self || pid <= 1 {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})
		if len(args) < 2 || filepath.Base(string(args[0])) != "ygg" || string(args[1]) != name {
			continue
		}
		// Signal 0 asks the kernel whether Loki may signal it at all.
		if err := syscall.Kill(pid, 0); err != nil {
			others++
			continue
		}
		mine = append(mine, pid)
	}
	switch {
	case len(mine) == 1:
		return mine, ""
	case len(mine) > 1:
		return nil, fmt.Sprintf("%d processes run as `ygg %s` beside Loki; it won't guess which to freeze", len(mine), name)
	case others > 0:
		return nil, fmt.Sprintf("%s runs as another user, and Loki may signal only its own processes", Title(name))
	}
	return nil, fmt.Sprintf("Loki can't see %s's process: it runs outside Loki's container (another PID namespace) or not at all", Title(name))
}

// Thaw is a detached thaw waiting to send SIGCONT.
type Thaw struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	done bool
}

// ScheduleThaw starts `sh -c 'sleep N; kill -CONT <pids>'` in a session of
// its own, so it outlives Loki and isn't stopped with Loki's process group.
// N is after, rounded up to whole seconds.
func ScheduleThaw(pids []int, after time.Duration) (*Thaw, error) {
	seconds := int((max(after, 0) + time.Second - 1) / time.Second)
	var list []string
	for _, pid := range pids {
		list = append(list, strconv.Itoa(pid))
	}
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep %d; kill -CONT %s 2>/dev/null", seconds, strings.Join(list, " ")))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	thaw := &Thaw{cmd: cmd}
	go func() {
		// Reaped while Loki lives; after that, whoever inherits it reaps it.
		_ = cmd.Wait()
		thaw.mu.Lock()
		thaw.done = true
		thaw.mu.Unlock()
	}()
	return thaw, nil
}

// Pid is the detached thaw's process.
func (t *Thaw) Pid() int {
	return t.cmd.Process.Pid
}

// Cancel calls the detached thaw off: its whole session, sleep and all.
func (t *Thaw) Cancel() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}

type frozen struct {
	part string
	pids []int
	thaw *Thaw
}

// freeze stops the part's process until after, for the strike: the
// detached thaw first, so nothing is ever stopped without one.
func (l *Loki) freeze(id, name string, after time.Duration) ([]int, error) {
	pids, why := l.processes(name)
	if len(pids) == 0 {
		return nil, refuse(409, "%s can't be frozen: %s", Title(name), why)
	}
	if slices.Contains(pids, os.Getpid()) {
		return nil, refuse(403, "Loki never freezes itself")
	}
	thaw, err := l.scheduleThaw(pids, after)
	if err != nil {
		return nil, fmt.Errorf("could not schedule %s's thaw, so it was not frozen: %w", Title(name), err)
	}
	for i, pid := range pids {
		if err := l.signal(pid, syscall.SIGSTOP); err != nil {
			for _, done := range pids[:i] {
				_ = l.signal(done, syscall.SIGCONT)
			}
			thaw.Cancel()
			return nil, fmt.Errorf("could not freeze %s (pid %d): %w", Title(name), pid, err)
		}
	}
	l.mu.Lock()
	l.frozen[id] = &frozen{part: name, pids: pids, thaw: thaw}
	l.mu.Unlock()
	return pids, nil
}

// thaw lets the strike's frozen process go now and calls its detached thaw off.
func (l *Loki) thaw(id string) {
	l.mu.Lock()
	f := l.frozen[id]
	delete(l.frozen, id)
	l.mu.Unlock()
	if f == nil {
		return
	}
	for _, pid := range f.pids {
		if err := l.signal(pid, syscall.SIGCONT); err != nil {
			l.Logf("thawing %s (pid %d): %v", f.part, pid, err)
		}
	}
	f.thaw.Cancel()
}

// ThawAll lets every frozen process go: when Loki shuts down.
func (l *Loki) ThawAll() {
	l.mu.Lock()
	var ids []string
	for id := range l.frozen {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		l.thaw(id)
	}
}
