package sindri

// A Claude Code session's transcript, for a widget on another machine: the
// session reader reads this machine's files itself, but a session on the VPS
// is only reachable through its Sindri.
//
//	GET /v1/sessions/claude/{id}/transcript?dir=<cwd>&tail=<bytes>&since=<size>:<modified>
//
// The end of the file, as text, with its size and when it last changed. With
// `since` set to what the last read said and the file unchanged, the text is
// left out (`unchanged`), so a reader polling over a tunnel moves no more
// than a few bytes while nothing happens. The owner only: a
// transcript holds everything said in the session.

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"yggdrasil/internal/part"
)

const (
	// TranscriptTail is how much of the end is read when the caller does not say.
	TranscriptTail = 1_500_000
	// TranscriptTailMax bounds what one request may ask for.
	TranscriptTailMax = 16_000_000
)

// sessionID is a Claude Code session id: no separators, so it cannot leave
// the projects folder.
var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Transcript is the end of one transcript.
type Transcript struct {
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	Text     string `json:"text"`
	// Unchanged is set, and Text empty, when the file is as `since` said.
	Unchanged bool `json:"unchanged,omitempty"`
}

// findTranscript is the session's file under projects: the folder its
// directory names first, then every project folder, as a session moved
// between folders is found by the widget.
func findTranscript(projects, id, dir string) (string, bool) {
	if dir != "" {
		guess := filepath.Join(projects, EncodeProjectDir(dir), id+".jsonl")
		if info, err := os.Stat(guess); err == nil && info.Mode().IsRegular() {
			return guess, true
		}
	}
	folders, err := os.ReadDir(projects)
	if err != nil {
		return "", false
	}
	for _, folder := range folders {
		if !folder.IsDir() {
			continue
		}
		path := filepath.Join(projects, folder.Name(), id+".jsonl")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}

// readTranscript reads the last tail bytes of the file at path.
func readTranscript(path string, tail int64) (Transcript, error) {
	file, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Transcript{}, err
	}
	start := max(info.Size()-tail, 0)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return Transcript{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, tail))
	if err != nil {
		return Transcript{}, err
	}
	return Transcript{Size: info.Size(), Modified: info.ModTime().UnixMilli(), Text: string(data)}, nil
}

func (s *Sindri) mountTranscripts(p *part.Part) {
	p.Handle("GET /v1/sessions/claude/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		if part.Caller(r) != part.Owner {
			part.Error(w, http.StatusForbidden, "only the owner may read a transcript")
			return
		}
		if s.Sessions == nil || s.Sessions.Claude.ClaudeDir == "" {
			part.Error(w, http.StatusServiceUnavailable, "Claude Code's sessions are off here")
			return
		}
		id := r.PathValue("id")
		if !sessionID.MatchString(id) {
			part.Error(w, http.StatusBadRequest, "not a session id")
			return
		}
		tail := int64(TranscriptTail)
		if asked := r.URL.Query().Get("tail"); asked != "" {
			n, err := strconv.ParseInt(asked, 10, 64)
			if err != nil || n <= 0 {
				part.Error(w, http.StatusBadRequest, "`tail` must be a number of bytes")
				return
			}
			tail = min(n, TranscriptTailMax)
		}
		path, ok := findTranscript(filepath.Join(s.Sessions.Claude.ClaudeDir, "projects"), id, r.URL.Query().Get("dir"))
		if !ok {
			part.Error(w, http.StatusNotFound, "no transcript for that session here")
			return
		}
		if since := r.URL.Query().Get("since"); since != "" {
			if info, err := os.Stat(path); err == nil && since == strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixMilli(), 10) {
				part.JSON(w, http.StatusOK, Transcript{Size: info.Size(), Modified: info.ModTime().UnixMilli(), Unchanged: true})
				return
			}
		}
		transcript, err := readTranscript(path, tail)
		if errors.Is(err, os.ErrNotExist) {
			part.Error(w, http.StatusNotFound, "no transcript for that session here")
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		part.JSON(w, http.StatusOK, transcript)
	})
}
