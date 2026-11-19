package heimdall

// files: the settings and token files, ~/.config/yggdrasil/* and the files
// named like .env in each repo's root (plus HEIMDALL_FILES), that the group
// or others may read, and token files not changed in longer than
// tokenAgeDays, whose tokens are worth rotating.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Files is the files check.
type Files struct {
	// Dirs are folders whose every file holds settings or tokens (~/.config/yggdrasil).
	Dirs []string
	// Globs are more files to look at (HEIMDALL_FILES).
	Globs []string
	// Home is shown as ~ in where.
	Home string
}

// Name is "files".
func (*Files) Name() string { return "files" }

// TokenFile is a file name that holds tokens: *.token, *.env, .env*.
func TokenFile(name string) bool {
	return strings.HasSuffix(name, ".token") || strings.HasSuffix(name, ".key") || EnvLike(name)
}

var tokenSetting = regexp.MustCompile(`(?m)^\s*(export\s+)?[A-Za-z0-9_]*(TOKEN|TOKENS|KEY|SECRET)\s*=\s*\S`)

// holdsTokens is a .token file, or a settings file that sets a token, key or
// secret to something. Only whether it does is read, never the value.
func holdsTokens(path string) bool {
	if strings.HasSuffix(path, ".token") || strings.HasSuffix(path, ".key") {
		return true
	}
	data, err := os.ReadFile(path)
	return err == nil && len(data) < 1<<20 && tokenSetting.Match(data)
}

// Run looks at each file's mode and age.
func (c *Files) Run(_ context.Context, env *Env) (Result, error) {
	findings := []Finding{}
	seen := map[string]bool{}
	look := func(path string, repo string) {
		path = filepath.Clean(path)
		if seen[path] || strings.HasPrefix(path, "/mnt/") { // Windows drives show every file as 0777
			return
		}
		seen[path] = true
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		findings = append(findings, c.look(path, info, repo, env)...)
	}
	for _, dir := range c.Dirs {
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		findings = append(findings, c.mode(dir, info, "")...)
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if entry.Type().IsRegular() && !strings.HasSuffix(entry.Name(), ".tmp") {
				look(filepath.Join(dir, entry.Name()), "")
			}
		}
	}
	for _, repo := range env.Repos {
		entries, _ := os.ReadDir(repo.Path)
		for _, entry := range entries {
			if entry.Type().IsRegular() && EnvLike(entry.Name()) {
				look(filepath.Join(repo.Path, entry.Name()), repo.Name)
			}
		}
	}
	for _, glob := range c.Globs {
		matches, _ := filepath.Glob(c.expand(glob))
		for _, match := range matches {
			look(match, "")
		}
	}
	return Result{Findings: findings}, nil
}

func (c *Files) expand(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok && c.Home != "" {
		return filepath.Join(c.Home, rest)
	}
	return path
}

func (c *Files) shown(path string) string {
	if c.Home != "" {
		if rest, ok := strings.CutPrefix(path, c.Home+"/"); ok {
			return "~/" + rest
		}
	}
	return path
}

func (c *Files) look(path string, info fs.FileInfo, repo string, env *Env) []Finding {
	findings := c.mode(path, info, repo)
	if !info.Mode().IsRegular() || !TokenFile(filepath.Base(path)) || env.TokenAge <= 0 || !holdsTokens(path) {
		return findings
	}
	age := env.Now.Sub(info.ModTime())
	if age <= env.TokenAge {
		return findings
	}
	days := int(age / (24 * time.Hour))
	return append(findings, Finding{
		ID:       StableID("age", c.shown(path)),
		Severity: Low,
		Title:    fmt.Sprintf("%s hasn't changed in %d days", c.shown(path), days),
		Detail:   fmt.Sprintf("The tokens in it are at least %d days old, older than the %d days set.", days, int(env.TokenAge/(24*time.Hour))),
		Where:    c.shown(path),
		Fix:      "Rotate the tokens in it: make new ones, put them here and where they are checked, then restart what reads it.",
		Repo:     repo,
	})
}

// mode is a finding when the group or others may read (or write) the file.
func (c *Files) mode(path string, info fs.FileInfo, repo string) []Finding {
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return nil
	}
	who, severity := "your group", Medium
	if perm&0o007 != 0 {
		who, severity = "every user on this machine", High
	}
	what, fix := "file", fmt.Sprintf("chmod 600 %s", c.shown(path))
	if info.IsDir() {
		what, fix = "folder", fmt.Sprintf("chmod 700 %s", c.shown(path))
	}
	return []Finding{{
		ID:       StableID("mode", c.shown(path)),
		Severity: severity,
		Title:    fmt.Sprintf("%s can be read by %s", c.shown(path), who),
		Detail:   fmt.Sprintf("The %s holds settings or tokens and its mode is %04o; only you should read it.", what, perm),
		Where:    c.shown(path),
		Fix:      fix,
		Repo:     repo,
	}}
}
