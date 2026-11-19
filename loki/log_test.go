package loki

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheStrikeLogKeepsEveryEndedStrikeOnce(t *testing.T) {
	dir := t.TempDir()
	l := New(nil, nil, nil, nil)
	l.StatePath, l.LogPath = filepath.Join(dir, "loki.json"), filepath.Join(dir, "loki-strikes.jsonl")
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	add := func(i int, state string) {
		l.mu.Lock()
		l.strikes = append([]*Strike{{ID: fmt.Sprintf("s%03d", i), Part: "norn", Trick: Trick{Via: ViaSwitch, Kind: "health"}, StartedAt: at.Add(time.Duration(i) * time.Hour), State: state}}, l.strikes...)
		l.trimLocked()
		l.mu.Unlock()
		l.save()
	}
	for i := range KeepStrikes + 5 {
		add(i, Caught)
	}
	add(KeepStrikes+5, Striking)
	l.save() // saved again while running: not logged

	log := l.Log()
	if len(log) != KeepStrikes+5 || log[0].ID != "s000" || log[len(log)-1].ID != fmt.Sprintf("s%03d", KeepStrikes+4) {
		t.Fatalf("%d logged, first %s", len(log), log[0].ID)
	}
	if len(l.Strikes(1000)) != KeepStrikes {
		t.Fatalf("state keeps %d", len(l.Strikes(1000)))
	}

	// A restart reads the log back and adds nothing twice; the running strike
	// ends as stopped and is logged then.
	again := New(nil, nil, nil, nil)
	again.StatePath, again.LogPath = l.StatePath, l.LogPath
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	log = again.Log()
	if len(log) != KeepStrikes+6 || log[len(log)-1].State != Stopped {
		t.Fatalf("after a restart: %d logged, last %+v", len(log), log[len(log)-1])
	}
	text, _ := os.ReadFile(l.LogPath)
	if lines := len(splitLines(text)); lines != KeepStrikes+6 {
		t.Fatalf("%d lines in the file", lines)
	}
}

func TestALokiFromBeforeTheLogLogsTheStrikesItKept(t *testing.T) {
	dir := t.TempDir()
	l := New(nil, nil, nil, nil)
	l.StatePath = filepath.Join(dir, "loki.json")
	l.strikes = []*Strike{{ID: "b", State: Missed, Trick: Trick{Kind: "health"}}, {ID: "a", State: Caught, Trick: Trick{Kind: "health"}}}
	l.save()

	again := New(nil, nil, nil, nil)
	again.StatePath, again.LogPath = l.StatePath, filepath.Join(dir, "loki-strikes.jsonl")
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if log := again.Log(); len(log) != 2 || log[0].ID != "a" || log[1].ID != "b" {
		t.Fatalf("logged %+v", log)
	}
}

func splitLines(text []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range text {
		if c == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	return lines
}
