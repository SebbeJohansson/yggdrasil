package loki

// The strike log: every strike that ended, once, one JSON line each, oldest
// first, never trimmed. Loki keeps only its last KeepStrikes strikes in its
// state; the log is what remembers the rest (the widget's tree draws a raven
// for each). Lines are appended, never rewritten.

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"yggdrasil/internal/part"
)

// loadLog reads the log into memory. Call it with l.mu held.
func (l *Loki) loadLog() error {
	l.logged = map[string]bool{}
	l.log = nil
	if l.LogPath == "" {
		return nil
	}
	file, err := os.Open(l.LogPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for lines.Scan() {
		var strike Strike
		if json.Unmarshal(lines.Bytes(), &strike) != nil || strike.ID == "" || l.logged[strike.ID] {
			continue // a torn last line from a crash: skipped, the strike is logged again
		}
		l.logged[strike.ID] = true
		l.log = append(l.log, strike)
	}
	return lines.Err()
}

// logEnded appends every ended strike not yet in the log, oldest first.
// strikes is newest first, as Loki keeps them.
func (l *Loki) logEnded(strikes []Strike) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	var fresh []Strike
	l.mu.Lock()
	for i := len(strikes) - 1; i >= 0; i-- {
		if s := strikes[i]; s.done() && !l.logged[s.ID] {
			s.Pids = nil
			fresh = append(fresh, s)
			l.logged[s.ID] = true // held back below if the append fails
		}
	}
	l.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	if l.LogPath != "" {
		if err := appendLines(l.LogPath, fresh); err != nil {
			l.Logf("keeping the strike log: %v", err)
			l.mu.Lock()
			for _, s := range fresh {
				delete(l.logged, s.ID)
			}
			l.mu.Unlock()
			return
		}
	}
	l.mu.Lock()
	l.log = append(l.log, fresh...)
	l.mu.Unlock()
}

func appendLines(path string, strikes []Strike) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var text []byte
	for _, s := range strikes {
		line, err := json.Marshal(s)
		if err != nil {
			file.Close()
			return err
		}
		text = append(append(text, line...), '\n')
	}
	if _, err := file.Write(text); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// Log is every strike that ended, oldest first.
func (l *Loki) Log() []Strike {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := make([]Strike, 0, len(l.log))
	for _, s := range l.log {
		list = append(list, s.copy())
	}
	return list
}

func (l *Loki) strikeLog(w http.ResponseWriter, _ *http.Request) {
	part.JSON(w, http.StatusOK, map[string]any{"strikes": l.Log()})
}
