package main

// `ygg pair` shows a pairing link and a QR code for this machine, so another
// device (the widget, the phone, another machine's terminal) is connected by
// scanning or pasting instead of copying owner.token by hand. `ygg pair add
// <link>` takes such a link and adds the node to nodes.json. The link:
//
//	yggdrasil://pair?v=1&id=<id>&label=<label>&host=<tailnet name>&token=<owner token>
//
// It holds the owner token, so it is shown only on this screen and never
// stored. A work machine makes none, and a link from one is refused.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

func init() {
	tellCommands["pair"] = struct {
		about string
		run   func(args []string) int
	}{"connect another device to this machine with a QR code: ygg pair [--id x] [--label x] [--host x] [--no-qr] [--json]; ygg pair add <link>", pair}
}

var pairID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// pairing is what a link carries.
type pairing struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Host  string `json:"host"`
	Token string `json:"token"`
	Work  bool   `json:"work,omitempty"`
}

func (p pairing) link() string {
	query := url.Values{"v": {"1"}, "id": {p.ID}, "label": {p.Label}, "host": {p.Host}, "token": {p.Token}}
	if p.Work {
		query.Set("work", "1")
	}
	return "yggdrasil://pair?" + query.Encode()
}

// parsePairing reads a link, and refuses one that is not usable.
func parsePairing(link string) (pairing, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "yggdrasil" || u.Host != "pair" {
		return pairing{}, errors.New("not a pairing link (yggdrasil://pair?...)")
	}
	q := u.Query()
	if q.Get("v") != "1" {
		return pairing{}, fmt.Errorf("pairing link version %q is not one this ygg knows (1)", q.Get("v"))
	}
	p := pairing{ID: q.Get("id"), Label: q.Get("label"), Host: q.Get("host"), Token: q.Get("token"), Work: q.Get("work") == "1"}
	switch {
	case !pairID.MatchString(p.ID):
		return pairing{}, errors.New("the link has no usable id (a-z, 0-9, -)")
	case p.Host == "" || strings.ContainsAny(p.Host, "/:? "):
		return pairing{}, errors.New("the link has no usable host (a tailnet name, no scheme or port)")
	case p.Token == "":
		return pairing{}, errors.New("the link has no token")
	}
	if p.Label == "" {
		p.Label = p.ID
	}
	return p, nil
}

// workMachine says whether this is a work machine: YGG_WORK=on in the
// environment or in parts.env, which is where the parts get it.
func workMachine() bool {
	if os.Getenv("YGG_WORK") == "on" {
		return true
	}
	data, err := os.ReadFile(filepath.Join(configDir(), "parts.env"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "YGG_WORK=on" {
			return true
		}
	}
	return false
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config/yggdrasil")
}

// tailnetHost is this machine's name on the tailnet, from Tailscale.
func tailnetHost() string {
	for _, bin := range []string{"tailscale", "tailscale.exe"} {
		data, err := exec.Command(bin, "status", "--json").Output()
		if err != nil {
			continue
		}
		var status struct {
			Self struct{ DNSName string }
		}
		if json.Unmarshal(data, &status) == nil && status.Self.DNSName != "" {
			return strings.TrimSuffix(status.Self.DNSName, ".")
		}
	}
	return ""
}

func pair(args []string) int {
	if len(args) > 0 && args[0] == "add" {
		return pairAdd(args[1:])
	}
	hostname, _ := os.Hostname()
	set := flag.NewFlagSet("pair", flag.ContinueOnError)
	id := set.String("id", "", "the id the other device gives this machine (default: its hostname)")
	label := set.String("label", "", "the name the other device shows (default: the id)")
	host := set.String("host", "", "this machine's tailnet name (default: from tailscale)")
	noQR := set.Bool("no-qr", false, "print the link only")
	asJSON := set.Bool("json", false, "print the link and its parts as JSON")
	if words, err := flags(set, args); err != nil || len(words) > 0 {
		fmt.Fprintln(os.Stderr, "usage: ygg pair [--id x] [--label x] [--host x] [--no-qr] [--json]\n       ygg pair add <link>")
		return 2
	}
	if workMachine() {
		return failed("pair", errors.New("this is a work machine (YGG_WORK=on): its token never goes to another device"))
	}
	token := ownerToken()
	if token == "" {
		return failed("pair", errors.New("no owner token: set YGG_TOKEN or run scripts/install.sh"))
	}
	if *host == "" {
		*host = tailnetHost()
	}
	if *host == "" {
		return failed("pair", errors.New("no tailnet name: is Tailscale up (docs/tailscale.md)? or give --host <name>.<tailnet>.ts.net"))
	}
	if *id == "" {
		*id = strings.ToLower(strings.Split(hostname, ".")[0])
	}
	p := pairing{ID: *id, Label: *label, Host: *host, Token: token}
	if p.Label == "" {
		p.Label = p.ID
	}
	if _, err := parsePairing(p.link()); err != nil {
		return failed("pair", err)
	}
	if *asJSON {
		return printJSON(map[string]any{"uri": p.link(), "id": p.ID, "label": p.Label, "host": p.Host, "token": p.Token})
	}
	if !*noQR {
		code, err := qrcode.New(p.link(), qrcode.Medium)
		if err != nil {
			return failed("pair", err)
		}
		fmt.Fprint(out, qrText(code.Bitmap()))
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "Scan it in the widget or the phone app, or on another machine run:\n  ygg pair add '%s'\n", p.link())
	fmt.Fprintln(out, "It holds this machine's owner token: show it only to your own devices.")
	return 0
}

// qrText draws a QR bitmap with half blocks, two rows to a line, with the
// quiet border, dark on light so it scans on a dark terminal.
func qrText(bitmap [][]bool) string {
	n := len(bitmap)
	dark := func(x, y int) bool { return y >= 0 && y < n && x >= 0 && x < n && bitmap[y][x] }
	var b strings.Builder
	for y := -2; y < n+2; y += 2 {
		for x := -2; x < n+2; x++ {
			switch top, bottom := dark(x, y), dark(x, y+1); {
			case top && bottom:
				b.WriteString(" ")
			case top:
				b.WriteString("▄")
			case bottom:
				b.WriteString("▀")
			default:
				b.WriteString("█")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// pairAdd puts the node a link names into nodes.json.
func pairAdd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: ygg pair add <link>")
		return 2
	}
	p, err := parsePairing(args[0])
	if err != nil {
		return failed("pair add", err)
	}
	if p.Work {
		return failed("pair add", errors.New("that link is from a work machine; its token does not go on another device"))
	}
	if workMachine() {
		// A work machine may read personal ones, but its parts never dial the tailnet.
		fmt.Fprintln(os.Stderr, "note: this is a work machine; its widget may read this node, its parts will not connect to it")
	}
	path := filepath.Join(configDir(), "nodes.json")
	file := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &file); err != nil {
			return failed("pair add", fmt.Errorf("%s is not valid JSON: %w", path, err))
		}
	}
	nodes, _ := file["nodes"].([]any)
	entry := map[string]any{"id": p.ID, "label": p.Label, "token": p.Token, "tailnet": p.Host}
	replaced := false
	for i, node := range nodes {
		if m, ok := node.(map[string]any); ok && m["id"] == p.ID {
			// Keep what the entry has besides these (tunnel, ssh, parts).
			for k, v := range m {
				if _, set := entry[k]; !set {
					entry[k] = v
				}
			}
			nodes[i], replaced = entry, true
		}
	}
	if !replaced {
		nodes = append(nodes, entry)
	}
	file["nodes"] = nodes
	data, _ := json.MarshalIndent(file, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return failed("pair add", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return failed("pair add", err)
	}
	_ = os.Chmod(path, 0o600)
	verb := "added"
	if replaced {
		verb = "updated"
	}
	fmt.Fprintf(out, "%s node %q (%s) in %s; restart the widget to see it.\n", verb, p.ID, p.Host, path)
	return 0
}
