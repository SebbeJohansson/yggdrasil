package heimdall

// listening: every TCP listener on the host that is not bound to loopback,
// from /proc/net/tcp and tcp6, with the process that holds it when this user
// may see its open files. Yggdrasil's own ports beyond loopback are high.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"yggdrasil/internal/where"
)

// Listener is one listening TCP socket.
type Listener struct {
	IP    net.IP
	Port  int
	Inode string
}

// Addr is ip:port, IPv6 in brackets.
func (l Listener) Addr() string {
	return net.JoinHostPort(l.IP.String(), strconv.Itoa(l.Port))
}

// Loopback is whether only this machine can reach it.
func (l Listener) Loopback() bool { return l.IP.IsLoopback() }

// Listening is the listening check.
type Listening struct {
	// Proc is /proc; a fake folder in tests.
	Proc string
	// Ports are Yggdrasil's own ports, each part's and its Loki proxy's.
	Ports map[int]string
}

// NewListening reads proc (/proc).
func NewListening(proc string) *Listening {
	return &Listening{Proc: proc, Ports: OwnPorts()}
}

// OwnPorts are every part's port and its Loki proxy's (port + 100), by port.
func OwnPorts() map[int]string {
	ports := map[int]string{}
	for name, port := range where.Ports {
		ports[port] = name
		if name != "loki" {
			ports[port+100] = "Loki's proxy for " + name
		}
	}
	return ports
}

// Name is "listening".
func (*Listening) Name() string { return "listening" }

// Run lists the listeners beyond loopback.
func (c *Listening) Run(_ context.Context, _ *Env) (Result, error) {
	var listeners []Listener
	read := 0
	for _, file := range []string{"net/tcp", "net/tcp6"} {
		data, err := os.ReadFile(filepath.Join(c.Proc, file))
		if err != nil {
			continue
		}
		read++
		listeners = append(listeners, ParseProcNetTCP(data)...)
	}
	if read == 0 {
		return Result{}, fmt.Errorf("can't read %s/net/tcp: run Heimdall on the host, not in a container", c.Proc)
	}
	owners := SocketOwners(c.Proc)
	findings := []Finding{}
	seen := map[string]bool{}
	for _, l := range listeners {
		if l.Loopback() || seen[l.Addr()] {
			continue
		}
		seen[l.Addr()] = true
		findings = append(findings, c.finding(l, owners[l.Inode]))
	}
	return Result{Findings: findings}, nil
}

func (c *Listening) finding(l Listener, process string) Finding {
	who := process
	if who == "" {
		who = "a process this user can't see (another user's, or root's)"
	}
	f := Finding{
		ID:     StableID(l.Addr()),
		Where:  l.Addr(),
		Detail: fmt.Sprintf("Port %d listens on %s, held by %s.", l.Port, l.IP, who),
	}
	wildcard := l.IP.IsUnspecified()
	switch part, own := c.Ports[l.Port]; {
	case own:
		f.Severity, f.Critical = High, true
		f.Title = fmt.Sprintf("Yggdrasil's %s listens beyond loopback on %s", part, l.Addr())
		f.Detail += " Every part should listen on 127.0.0.1 only: anything that reaches this machine can now try its tokens."
		f.Fix = fmt.Sprintf("Set its <PART>_ADDR (or LOKI_PROXY_ADDR_<PART>) back to 127.0.0.1:%d and restart it.", l.Port)
	case wildcard:
		f.Severity = Medium
		f.Title = fmt.Sprintf("Port %d listens on every address (%s)", l.Port, l.Addr())
		f.Fix = fmt.Sprintf("Bind it to 127.0.0.1 if only this machine uses it; for a container, publish it as 127.0.0.1:%d:%d.", l.Port, l.Port)
	default:
		f.Severity = Low
		f.Title = fmt.Sprintf("Port %d listens on %s", l.Port, l.Addr())
		f.Fix = "Bind it to 127.0.0.1 if nothing else needs to reach it."
	}
	if process != "" {
		f.Title += " (" + process + ")"
	}
	return f
}

// ParseProcNetTCP reads /proc/net/tcp or tcp6: the sockets in LISTEN (0A).
func ParseProcNetTCP(data []byte) []Listener {
	var listeners []Listener
	scanner := bufio.NewScanner(bytes.NewReader(data))
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		host, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ip := parseHexIP(host)
		port, err := strconv.ParseUint(portHex, 16, 16)
		if ip == nil || err != nil {
			continue
		}
		listeners = append(listeners, Listener{IP: ip, Port: int(port), Inode: fields[9]})
	}
	return listeners
}

// parseHexIP reads the kernel's address: 4 or 16 bytes, each 32-bit word in
// the host's (little-endian) order.
func parseHexIP(text string) net.IP {
	raw, err := hex.DecodeString(text)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil
	}
	ip := make(net.IP, len(raw))
	for word := 0; word < len(raw); word += 4 {
		for i := 0; i < 4; i++ {
			ip[word+i] = raw[word+3-i]
		}
	}
	if v4 := ip.To4(); v4 != nil && len(raw) == 16 && !ip.IsUnspecified() {
		return v4 // ::ffff:a.b.c.d
	}
	return ip
}

// SocketOwners maps a socket's inode to the process holding it, "name (pid
// N)", for every process this user may read the open files of.
func SocketOwners(proc string) map[string]string {
	owners := map[string]string{}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return owners
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(proc, entry.Name(), "fd"))
		if err != nil {
			continue
		}
		name := processName(proc, entry.Name())
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(proc, entry.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if inode, ok := strings.CutPrefix(link, "socket:["); ok {
				inode = strings.TrimSuffix(inode, "]")
				if _, known := owners[inode]; !known {
					owners[inode] = fmt.Sprintf("%s (pid %d)", name, pid)
				}
			}
		}
	}
	return owners
}

// processName is the command, with the part for `ygg <part>`.
func processName(proc, pid string) string {
	cmdline, _ := os.ReadFile(filepath.Join(proc, pid, "cmdline"))
	args := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})
	if len(args) >= 2 && filepath.Base(string(args[0])) == "ygg" {
		return "ygg " + string(args[1])
	}
	comm, _ := os.ReadFile(filepath.Join(proc, pid, "comm"))
	if name := strings.TrimSpace(string(comm)); name != "" {
		return name
	}
	if len(args) > 0 && len(args[0]) > 0 {
		return filepath.Base(string(args[0]))
	}
	return "pid " + pid
}
