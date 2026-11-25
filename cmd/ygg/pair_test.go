package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairingRoundTrip(t *testing.T) {
	p := pairing{ID: "vps", Label: "My VPS & co", Host: "vps.tail1.ts.net", Token: "a+b/c=="}
	got, err := parsePairing(p.link())
	if err != nil || got != p {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for _, bad := range []string{"https://x", "yggdrasil://pair?v=2&id=a&host=h&token=t", "yggdrasil://pair?v=1&id=A&host=h&token=t",
		"yggdrasil://pair?v=1&id=a&host=h:1&token=t", "yggdrasil://pair?v=1&id=a&host=h"} {
		if _, err := parsePairing(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPairAddKeepsTheRest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("YGG_WORK", "")
	dir := filepath.Join(home, ".config/yggdrasil")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "nodes.json"), []byte(`{"label":"Me","nodes":[{"id":"vps","label":"old","token":"x","ssh":"vps"}]}`), 0o600)
	var buf bytes.Buffer
	out = &buf
	defer func() { out = os.Stdout }()
	link := pairing{ID: "vps", Label: "VPS", Host: "vps.t.ts.net", Token: "tok"}.link()
	if code := pairAdd([]string{link}); code != 0 {
		t.Fatalf("exit %d: %s", code, buf.String())
	}
	if code := pairAdd([]string{pairing{ID: "home", Host: "h.t.ts.net", Token: "t2"}.link()}); code != 0 {
		t.Fatal("second add")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "nodes.json"))
	var file struct {
		Label string
		Nodes []map[string]any
	}
	json.Unmarshal(data, &file)
	if file.Label != "Me" || len(file.Nodes) != 2 || file.Nodes[0]["token"] != "tok" || file.Nodes[0]["ssh"] != "vps" || file.Nodes[0]["tailnet"] != "vps.t.ts.net" {
		t.Fatalf("nodes.json: %s", data)
	}
	if info, _ := os.Stat(filepath.Join(dir, "nodes.json")); info.Mode().Perm() != 0o600 {
		t.Error("not chmod 600")
	}
}

func TestPairRefusesWork(t *testing.T) {
	t.Setenv("YGG_WORK", "on")
	t.Setenv("YGG_TOKEN", "t")
	if code := pair([]string{"--host", "h.t.ts.net"}); code == 0 {
		t.Error("made a link on a work machine")
	}
	if code := pairAdd([]string{pairing{ID: "w", Host: "h", Token: "t", Work: true}.link()}); code == 0 {
		t.Error("added a work link")
	}
}

func TestPairPrintsQR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("YGG_WORK", "")
	t.Setenv("YGG_TOKEN", "secret")
	var buf bytes.Buffer
	out = &buf
	defer func() { out = os.Stdout }()
	if code := pair([]string{"--host", "vps.t.ts.net", "--id", "vps"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(buf.String(), "█") || !strings.Contains(buf.String(), "yggdrasil://pair?") {
		t.Errorf("output: %s", buf.String())
	}
}
