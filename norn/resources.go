package norn

// Resource links for new meeting notes. Your notes start with a line linking
// the notes in the vault's Resources folder they belong to:
//
//	[[Northwind]], [[Acme]]
//
// A note Norn makes gets that line too. It is picked the way you pick it: a
// resource named in the subject or by an attendee ("Northwind Team"), and
// the resources on the first line of your earlier notes for the same people
// or subject. With nothing to go on, the resources most of your recent notes
// carry. Only titles and those first lines are read.

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	// resourcesFolder is where the notes a meeting note links to are.
	resourcesFolder = "Resources"
	// maxVaultTitles is how many notes are read for their titles, to tell whether [[Title]] is unique.
	maxVaultTitles = 20000
	// resourceNotes is how many related earlier notes are read for their resource line.
	resourceNotes = 8
	// recentResourceNotes is how many of your newest notes give the resources when nothing is related.
	recentResourceNotes = 20
)

// resource is a note in Resources a meeting note can link to.
type resource struct {
	Title string
	// Target is what goes inside [[ ]]: the title, or the path when another note shares it.
	Target string
}

// Resources is the notes in the vault's Resources folder, longest title first.
func (v *Vault) Resources(now time.Time) []resource {
	if now.Sub(v.resourcesScanned) < vaultScanEvery && v.resources != nil {
		return v.resources
	}
	titles := map[string]int{}
	var found []string
	count := 0
	_ = filepath.WalkDir(v.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != v.Dir && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		if count++; count > maxVaultTitles {
			return filepath.SkipAll
		}
		titles[strings.ToLower(strings.TrimSuffix(name, ".md"))]++
		if rel, err := filepath.Rel(v.Dir, path); err == nil && strings.HasPrefix(filepath.ToSlash(rel), resourcesFolder+"/") {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	out := []resource{}
	for _, path := range found {
		title := strings.TrimSuffix(filepath.Base(path), ".md")
		target := title
		if titles[strings.ToLower(title)] > 1 {
			target = strings.TrimSuffix(path, ".md")
		}
		out = append(out, resource{Title: title, Target: target})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Title) != len(out[j].Title) {
			return len(out[i].Title) > len(out[j].Title)
		}
		return out[i].Target < out[j].Target
	})
	v.resources, v.resourcesScanned = out, now
	return out
}

// resourceLine is a line of nothing but [[links]], commas between them.
var resourceLine = regexp.MustCompile(`^\s*\[\[[^\]\n]+\]\](\s*,?\s*\[\[[^\]\n]+\]\])*\s*,?\s*$`)

var wikiTarget = regexp.MustCompile(`\[\[([^\]|#\n]+)[^\]\n]*\]\]`)

// headResources is the resources on a note's first line, when that line is
// links only. Front matter is skipped.
func (v *Vault) headResources(path string, byTarget map[string]resource) []resource {
	file, err := os.Open(filepath.Join(v.Dir, filepath.FromSlash(path)))
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	front := false
	for line := 0; scanner.Scan() && line < 200; line++ {
		text := strings.TrimSpace(scanner.Text())
		if line == 0 && text == "---" {
			front = true
			continue
		}
		if front {
			front = text != "---"
			continue
		}
		if text == "" {
			continue
		}
		if !resourceLine.MatchString(text) {
			return nil
		}
		var out []resource
		for _, match := range wikiTarget.FindAllStringSubmatch(text, -1) {
			if r, ok := byTarget[strings.ToLower(strings.TrimSpace(match[1]))]; ok && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
		return out
	}
	return nil
}

// namesResource is whether text names the resource's title as a whole word.
func namesResource(text, title string) bool {
	lower, want := fold(strings.ToLower(text)), fold(strings.ToLower(title))
	for from := 0; ; {
		at := strings.Index(lower[from:], want)
		if at < 0 {
			return false
		}
		at += from
		end := at + len(want)
		before := at == 0 || !isTitleByte(lower[at-1])
		after := end == len(lower) || !isTitleByte(lower[end])
		if before && after {
			return true
		}
		from = at + 1
	}
}

func isTitleByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b >= 0x80
}

// MeetingResources is the resources a new note for the meeting links to,
// most likely first. `me` is your own names, left out of the matching.
func (v *Vault) MeetingResources(meeting Meeting, me []string, now time.Time) []resource {
	all := v.Resources(now)
	if len(all) == 0 {
		return nil
	}
	byTarget := map[string]resource{}
	for _, r := range all {
		byTarget[strings.ToLower(r.Target)] = r
		if _, taken := byTarget[strings.ToLower(r.Title)]; !taken {
			byTarget[strings.ToLower(r.Title)] = r
		}
	}
	score := map[resource]int{}
	var order []resource
	add := func(r resource, points int) {
		if _, seen := score[r]; !seen {
			order = append(order, r)
		}
		score[r] += points
	}

	people := othersThan(meeting.Attendees, me)
	subject := withoutNames(meeting.Subject, me)
	named := map[resource]bool{}
	for _, r := range all {
		if namesResource(subject, r.Title) || slices.ContainsFunc(people, func(p string) bool { return namesResource(p, r.Title) }) {
			named[r] = true
			add(r, 0)
		}
	}

	// The resources most of your related notes carry.
	withLine := 0
	for _, note := range v.Related(people, subject, "", resourceNotes, now) {
		if found := v.headResources(note.Path, byTarget); len(found) > 0 {
			withLine++
			for _, r := range found {
				add(r, 1)
			}
		}
	}
	// Nothing related: the resources most of your recent notes carry.
	if withLine == 0 {
		for _, note := range v.Notes(now) {
			if withLine == recentResourceNotes {
				break
			}
			if note.Date == "" {
				continue
			}
			if found := v.headResources(note.Path, byTarget); len(found) > 0 {
				withLine++
				for _, r := range found {
					add(r, 1)
				}
			}
		}
	}

	var out []resource
	for _, r := range order {
		if named[r] || withLine > 0 && score[r]*2 >= withLine {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if named[out[i]] != named[out[j]] {
			return named[out[i]]
		}
		return score[out[i]] > score[out[j]]
	})
	return out
}

// resourceLinks is the first line of a new note: "[[Northwind]], [[Acme]]".
func resourceLinks(resources []resource) string {
	links := make([]string, len(resources))
	for i, r := range resources {
		links[i] = "[[" + r.Target + "]]"
	}
	return strings.Join(links, ", ")
}
