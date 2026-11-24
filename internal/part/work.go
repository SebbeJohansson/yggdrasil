package part

// A work machine (YGG_WORK=on in parts.env) holds work data, and nothing of it
// may reach a personal machine. Personal machines are reached over the
// tailnet, so on a work machine every part refuses to connect to a tailnet
// address: Tailscale's 100.64.0.0/10 and fd7a:115c:a1e0::/48. The check is on
// the address actually dialed, after DNS, so a *.ts.net name is refused too.
// The internet (Microsoft, Google, Linear, Claude) and this machine stay
// reachable. Sága, which syncs to Mímir, does not start at all
// (saga.Main). docs/tailscale.md.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

// Work reports whether this machine is a work machine.
func Work() bool {
	return os.Getenv("YGG_WORK") == "on"
}

var tailnets = []*net.IPNet{
	mustCIDR("100.64.0.0/10"),
	mustCIDR("fd7a:115c:a1e0::/48"),
}

func mustCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return network
}

// OnTailnet reports whether address (host:port, an IP) is a tailnet address.
func OnTailnet(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range tailnets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// refuseTailnet is a dialer's Control: it runs after DNS, on the address
// about to be connected to.
func refuseTailnet(_, address string, _ syscall.RawConn) error {
	if OnTailnet(address) {
		return fmt.Errorf("work machine (YGG_WORK=on): %s is on the tailnet, where the personal machines are", address)
	}
	return nil
}

var guardOnce sync.Once

// GuardWork makes every HTTP connection this process opens through Go's
// default transport refuse the tailnet, on a work machine. New calls it.
func GuardWork() {
	if !Work() {
		return
	}
	guardOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: refuseTailnet}
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			transport.DialContext = dialer.DialContext
		}
	})
}
