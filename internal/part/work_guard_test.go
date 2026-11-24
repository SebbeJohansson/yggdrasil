package part

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnTailnet(t *testing.T) {
	for address, want := range map[string]bool{
		"100.101.102.103:443":      true,
		"100.64.0.1":               true,
		"100.127.255.254:80":       true,
		"[fd7a:115c:a1e0::1]:8772": true,
		"100.128.0.1:443":          false,
		"127.0.0.1:8772":           false,
		"10.0.0.5:80":              false,
		"[::1]:8772":               false,
		"vps.tail1234.ts.net:443":  false, // a name: checked once resolved
	} {
		if got := OnTailnet(address); got != want {
			t.Errorf("OnTailnet(%q) = %v", address, got)
		}
	}
}

func TestAWorkMachineRefusesTheTailnetButNotThisMachine(t *testing.T) {
	if err := refuseTailnet("tcp", "100.100.1.1:443", nil); err == nil || !strings.Contains(err.Error(), "YGG_WORK") {
		t.Fatalf("tailnet allowed: %v", err)
	}
	t.Setenv("YGG_WORK", "on")
	GuardWork()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer server.Close()
	res, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("loopback refused on a work machine: %v", err)
	}
	res.Body.Close()
	// A tailnet address is refused before any packet is sent.
	if _, err := http.Get("http://100.64.0.1:9/"); err == nil || !strings.Contains(err.Error(), "YGG_WORK") {
		t.Fatalf("a tailnet address was dialed on a work machine: %v", err)
	}
}
