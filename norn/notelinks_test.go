package norn

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinkSuggestions(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o755)
	os.MkdirAll(filepath.Join(dir, "Orgs"), 0o755)
	for _, f := range []string{"Northwind.md", "Orgs/Pris och Villkor.md", "Sprint review - 2026-11-17.md"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	v := OpenVault(dir, "V")
	targets := v.LinkTargets(time.Now())
	text := "Pratade med northwind om Pris och Villkor. Redan [[Northwind]] och `Northwind` och https://x.se/Northwind. Northwinds krav. Northwind igen."
	got := SuggestLinks(text, targets)
	if len(got) != 2 || got[0].Target != "Northwind" || got[0].Count != 2 || got[0].Wiki != "[[Northwind|northwind]]" {
		t.Fatalf("suggestions: %+v", got)
	}
	out, n := ApplyLink(text, targets, "Northwind")
	want := "Pratade med [[Northwind|northwind]] om Pris och Villkor. Redan [[Northwind]] och `Northwind` och https://x.se/Northwind. Northwinds krav. [[Northwind]] igen."
	if n != 2 || out != want {
		t.Fatalf("apply: %d %q", n, out)
	}
}
