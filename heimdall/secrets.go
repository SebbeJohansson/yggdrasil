package heimdall

// secrets: the lines added by each repo's new commits since the last run
// (the first run: the last 200), on every branch and tag, read for the
// shapes of common secrets; and files named like .env that git would commit
// (untracked and not ignored). A finding holds the file, line, kind and a
// masked preview, never the value. A secret in a commit stays a finding
// until that commit is gone from every branch and tag.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FirstCommits is how many commits the first run reads per repo.
const FirstCommits = 200

// MaxCommits is the most commits one later run reads per repo.
const MaxCommits = 2000

// IgnoreMarker on a line keeps it out of the secrets check.
const IgnoreMarker = "heimdall:ignore"

// Secrets is the secrets check.
type Secrets struct {
	// Git is the git command ("git").
	Git string
}

// NewSecrets uses git from PATH.
func NewSecrets() *Secrets { return &Secrets{Git: "git"} }

// Name is "secrets".
func (*Secrets) Name() string { return "secrets" }

// Shape is one kind of secret.
type Shape struct {
	Kind    string
	Pattern *regexp.Regexp
	// Group is the submatch holding the value; 0 is the whole match.
	Group int
	// MinEntropy, when set, is the bits per character the value needs.
	MinEntropy float64
}

// Shapes are the secrets looked for, most specific first.
var Shapes = []Shape{
	{Kind: "private key", Pattern: regexp.MustCompile(`-----BEGIN ((RSA|EC|DSA|OPENSSH|PGP|ENCRYPTED) )?PRIVATE KEY( BLOCK)?-----`)},
	{Kind: "GitHub token", Pattern: regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`)},
	{Kind: "Slack token", Pattern: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,250}`)},
	{Kind: "Anthropic key", Pattern: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{32,300}`)},
	{Kind: "OpenAI key", Pattern: regexp.MustCompile(`\bsk-(proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,300}`)},
	{Kind: "AWS access key", Pattern: regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{Kind: "secret assignment", Pattern: regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(token|key|secret)["']?\s*[:=]\s*["']?([A-Za-z0-9_+/=.~-]{20,500})`), Group: 2, MinEntropy: 3.5},
}

// Match is a secret found on one line.
type Match struct {
	Kind    string
	Preview string
}

// ScanLine finds the secrets on a line, at most one per kind, each with its
// masked preview only. Lines with IgnoreMarker are skipped.
func ScanLine(line string) []Match {
	if len(line) > 8192 || strings.Contains(line, IgnoreMarker) {
		return nil
	}
	var matches []Match
	var taken [][2]int
	for _, shape := range Shapes {
		for _, m := range shape.Pattern.FindAllStringSubmatchIndex(line, -1) {
			start, end := m[2*shape.Group], m[2*shape.Group+1]
			if start < 0 || slices.ContainsFunc(taken, func(span [2]int) bool { return start < span[1] && span[0] < end }) {
				continue
			}
			value := line[start:end]
			if shape.MinEntropy > 0 && (Entropy(value) < shape.MinEntropy || looksLikeCode(value)) {
				continue
			}
			taken = append(taken, [2]int{start, end})
			preview := Mask(value)
			if shape.Kind == "private key" {
				preview = "-----BEGIN … PRIVATE KEY-----"
			}
			matches = append(matches, Match{Kind: shape.Kind, Preview: preview})
			break
		}
	}
	return matches
}

// looksLikeCode is a value that is a name rather than a secret: a dotted
// path, an identifier made of words, or a made-up placeholder.
func looksLikeCode(value string) bool {
	if strings.Count(value, ".") >= 2 && !strings.ContainsAny(value, "+/=") {
		return true
	}
	return identifier.MatchString(value) || placeholder.MatchString(value)
}

// identifier is a name made of words: camelCase, SHOUTING_CASE, kebab-case.
var identifier = regexp.MustCompile(`^[a-z]+([A-Z][a-z]+)+$|^[A-Z_]+$|^[a-z]+([-_][a-z]+)*$`)

// placeholder is a value made up for a test or an example.
var placeholder = regexp.MustCompile(`(?i)0123456789|abcdefgh|example|changeme|placeholder|dummy|fake|xxxxxx|your[-_]`)

// Mask is a preview of a secret that gives none of it away: its first and
// last four characters when it is long enough, like ghp_…4f2a.
func Mask(value string) string {
	if len(value) < 16 {
		return "…"
	}
	return value[:4] + "…" + value[len(value)-4:]
}

// Entropy is a string's Shannon entropy, in bits per character.
func Entropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	var bits float64
	for _, c := range counts {
		p := c / n
		bits -= p * math.Log2(p)
	}
	return bits
}

// Run reads each repo's new commits and untracked files.
func (s *Secrets) Run(ctx context.Context, env *Env) (Result, error) {
	findings := []Finding{}
	var problems []string
	for _, repo := range env.Repos {
		found, err := s.repo(ctx, repo, env)
		if err != nil {
			problems = append(problems, repo.Name+": "+err.Error())
			// Keep what was found there before.
			for _, f := range env.Previous {
				if f.Repo == repo.Name {
					findings = append(findings, f)
				}
			}
			continue
		}
		findings = append(findings, found...)
	}
	if len(problems) > 0 && len(problems) == len(env.Repos) {
		return Result{}, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return Result{Findings: findings}, nil
}

func (s *Secrets) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Git, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

func (s *Secrets) repo(ctx context.Context, repo Repo, env *Env) ([]Finding, error) {
	refs, err := s.git(ctx, repo.Path, "for-each-ref", "--format=%(objectname)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return nil, err
	}
	tips := strings.Fields(refs)
	if head, err := s.git(ctx, repo.Path, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		tips = append(tips, strings.TrimSpace(head))
	}
	slices.Sort(tips)
	tips = slices.Compact(tips)

	// The commits since the last run: reachable now, not from what was read.
	var old []string
	for _, tip := range env.Memory[repo.Path] {
		if _, err := s.git(ctx, repo.Path, "cat-file", "-e", tip+"^{commit}"); err == nil {
			old = append(old, tip)
		}
	}
	findings := []Finding{}
	if len(tips) > 0 {
		args := []string{"log", "-p", "--no-color", "--no-ext-diff", "--no-renames", "--unified=0", "--format=" + commitMark + "%H", "--all"}
		if len(old) == 0 {
			args = append(args, "-n", strconv.Itoa(FirstCommits))
		} else {
			args = append(args, "-n", strconv.Itoa(MaxCommits), "--not")
			args = append(args, old...)
		}
		hits, err := s.scanLog(ctx, repo.Path, args)
		if err != nil {
			return nil, err
		}
		pushed := map[string]bool{}
		for _, hit := range hits {
			if _, known := pushed[hit.commit]; !known {
				pushed[hit.commit] = s.pushed(ctx, repo.Path, hit.commit)
			}
			findings = append(findings, commitFinding(repo, hit, pushed[hit.commit]))
		}
	}
	// Secrets found before stay while their commit is in history.
	for _, f := range env.Previous {
		if f.Repo != repo.Name || f.Commit == "" || slices.ContainsFunc(findings, func(n Finding) bool { return n.ID == f.ID }) {
			continue
		}
		contained, err := s.git(ctx, repo.Path, "for-each-ref", "--count=1", "--contains", f.Commit, "--format=%(refname)", "refs/heads", "refs/remotes", "refs/tags")
		if err != nil || strings.TrimSpace(contained) == "" {
			continue // gone from history: resolved
		}
		f.Critical = s.pushed(ctx, repo.Path, f.Commit)
		findings = append(findings, f)
	}
	env.Memory[repo.Path] = tips

	untracked, err := s.git(ctx, repo.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err == nil {
		for _, file := range strings.Split(untracked, "\x00") {
			if file != "" && EnvLike(path.Base(file)) {
				findings = append(findings, Finding{
					ID:       StableID(repo.Name, "untracked", file),
					Severity: Medium,
					Title:    fmt.Sprintf("%s in %s is not ignored, so it could be committed", file, repo.Name),
					Detail:   "A settings file named like .env is untracked but not in .gitignore: one `git add .` puts whatever it holds into history.",
					Where:    file,
					Fix:      fmt.Sprintf("Add %s to %s's .gitignore.", path.Base(file), repo.Name),
					Repo:     repo.Name,
				})
			}
		}
	}
	return findings, nil
}

// pushed is whether a remote-tracking branch holds the commit.
func (s *Secrets) pushed(ctx context.Context, dir, commit string) bool {
	out, err := s.git(ctx, dir, "for-each-ref", "--count=1", "--contains", commit, "--format=%(refname)", "refs/remotes")
	return err == nil && strings.TrimSpace(out) != ""
}

var envLike = regexp.MustCompile(`^(\.env(\..+)?|.+\.env)$`)

// EnvLike is a file name that holds settings: .env, .env.local, prod.env;
// not the examples (.env.example, .env.sample, .env.template, .env.dist).
func EnvLike(name string) bool {
	if !envLike.MatchString(name) {
		return false
	}
	for _, safe := range []string{"example", "sample", "template", "dist", "defaults"} {
		if strings.Contains(strings.ToLower(name), safe) {
			return false
		}
	}
	return true
}

const commitMark = "@@heimdall-commit "

type hit struct {
	commit, file string
	line         int
	Match
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// scanLog runs git log -p and reads the lines it adds.
func (s *Secrets) scanLog(ctx context.Context, dir string, args []string) ([]hit, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Git, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	hits := ParseLog(out)
	// Read git's output to the end whatever ParseLog did, or git blocks
	// writing into a full pipe and Wait never returns.
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git log: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return hits, nil
}

// ParseLog reads `git log -p --unified=0` with each commit marked, finding
// the secrets on added lines.
func ParseLog(r io.Reader) []hit {
	var hits []hit
	reader := bufio.NewReaderSize(r, 64*1024)
	commit, file := "", ""
	line, inHunk := 0, false
	for {
		chunk, more, err := reader.ReadLine()
		if err != nil {
			break
		}
		// A line longer than the buffer (a minified file, a lockfile) is
		// read past and only its first 64 KB scanned: it never ends the read.
		text := string(chunk)
		for more && err == nil {
			_, more, err = reader.ReadLine()
		}
		switch {
		case strings.HasPrefix(text, commitMark):
			commit, file, inHunk = strings.TrimPrefix(text, commitMark), "", false
		case strings.HasPrefix(text, "diff --git "):
			file, inHunk = "", false
		case !inHunk && strings.HasPrefix(text, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(text, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(text, "@@ "):
			if m := hunkHeader.FindStringSubmatch(text); m != nil {
				line, _ = strconv.Atoi(m[1])
				inHunk = true
			}
		case inHunk && strings.HasPrefix(text, "+"):
			if file != "" && commit != "" {
				for _, match := range ScanLine(text[1:]) {
					hits = append(hits, hit{commit: commit, file: file, line: line, Match: match})
				}
			}
			line++
		}
	}
	return hits
}

func commitFinding(repo Repo, h hit, pushed bool) Finding {
	short := h.commit
	if len(short) > 12 {
		short = short[:12]
	}
	where := fmt.Sprintf("%s:%d", h.file, h.line)
	state := "not pushed yet"
	if pushed {
		state = "pushed: it is on a remote"
	}
	return Finding{
		ID:       StableID(repo.Name, short, h.file, strconv.Itoa(h.line), strings.ReplaceAll(h.Kind, " ", "-")),
		Severity: High,
		Critical: pushed,
		Title:    fmt.Sprintf("A %s in %s's history (%s)", h.Kind, repo.Name, h.Preview),
		Detail:   fmt.Sprintf("Commit %s added a %s at %s, %s. Only a masked preview is kept: %s.", short, h.Kind, where, state, h.Preview),
		Where:    where,
		Fix:      "Rotate this key where it was issued, then remove it from history (git filter-repo) and force-push every branch that has it.",
		Repo:     repo.Name,
		Commit:   h.commit,
	}
}
