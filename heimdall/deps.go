package heimdall

// deps: the packages each repo pins (go.mod, yarn.lock, package-lock.json,
// or exact versions in a package.json without a lockfile), asked of
// osv.dev. Only each package's name, version and ecosystem leave the
// machine, and then the ids of the vulnerabilities osv.dev named, to read
// their summary, severity and fixed version.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OSVURL is osv.dev's public API.
const OSVURL = "https://api.osv.dev"

// UserAgent is what Heimdall calls itself outside: generic, naming nobody.
const UserAgent = "heimdall-deps-check/1"

// osvBatch is the most queries osv.dev takes in one querybatch.
const osvBatch = 1000

// Package is one pinned package in a repo.
type Package struct {
	Ecosystem string // Go or npm
	Name      string
	Version   string
	// File is the lockfile it was read from, relative to the repo.
	File string
}

// Deps is the deps check.
type Deps struct {
	// URL is osv.dev's API (OSVURL); a fake in tests.
	URL  string
	HTTP *http.Client
}

// NewDeps asks osv.dev at url.
func NewDeps(url string) *Deps {
	return &Deps{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Name is "deps".
func (*Deps) Name() string { return "deps" }

// Run reads every repo's packages and asks osv.dev about them.
func (d *Deps) Run(ctx context.Context, env *Env) (Result, error) {
	type owned struct {
		Package
		repo string
	}
	var all []owned
	for _, repo := range env.Repos {
		for _, pkg := range RepoPackages(repo.Path) {
			all = append(all, owned{pkg, repo.Name})
		}
	}
	if len(all) == 0 {
		return Result{Findings: []Finding{}}, nil
	}
	// One query per distinct package and version.
	type query struct{ ecosystem, name, version string }
	index := map[query]int{}
	var queries []query
	for _, p := range all {
		q := query{p.Ecosystem, p.Name, p.Version}
		if _, ok := index[q]; !ok {
			index[q] = len(queries)
			queries = append(queries, q)
		}
	}
	vulnsOf := make([][]string, len(queries))
	for start := 0; start < len(queries); start += osvBatch {
		end := min(start+osvBatch, len(queries))
		var body struct {
			Queries []map[string]any `json:"queries"`
		}
		for _, q := range queries[start:end] {
			body.Queries = append(body.Queries, map[string]any{
				"package": map[string]string{"name": q.name, "ecosystem": q.ecosystem},
				"version": q.version,
			})
		}
		var answer struct {
			Results []struct {
				Vulns []struct {
					ID string `json:"id"`
				} `json:"vulns"`
			} `json:"results"`
		}
		if err := d.call(ctx, http.MethodPost, "/v1/querybatch", body, &answer); err != nil {
			return Result{}, err
		}
		for i, result := range answer.Results {
			if start+i >= len(queries) {
				break
			}
			for _, v := range result.Vulns {
				vulnsOf[start+i] = append(vulnsOf[start+i], v.ID)
			}
		}
	}
	details := map[string]*Vuln{}
	var findings []Finding
	for _, p := range all {
		for _, id := range vulnsOf[index[query{p.Ecosystem, p.Name, p.Version}]] {
			vuln, ok := details[id]
			if !ok {
				vuln = &Vuln{ID: id}
				if err := d.call(ctx, http.MethodGet, "/v1/vulns/"+id, nil, vuln); err != nil {
					vuln = &Vuln{ID: id} // still a finding, with less to say
				}
				details[id] = vuln
			}
			findings = append(findings, vuln.Finding(p.Package, p.repo))
		}
	}
	if findings == nil {
		findings = []Finding{}
	}
	return Result{Findings: findings}, nil
}

func (d *Deps) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, d.URL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", UserAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := d.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("osv.dev: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("osv.dev answered %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(out)
}

// Vuln is the part of an osv.dev vulnerability Heimdall reads.
type Vuln struct {
	ID               string   `json:"id"`
	Summary          string   `json:"summary"`
	Details          string   `json:"details"`
	Aliases          []string `json:"aliases"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
	Affected []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Ranges []struct {
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
		DatabaseSpecific struct {
			Severity string `json:"severity"`
		} `json:"database_specific"`
	} `json:"affected"`
}

// Severity is low, medium or high from the database's own word: GitHub's
// LOW, MODERATE, HIGH, CRITICAL. Without one (Go's own database), medium.
func (v *Vuln) Severity() string {
	word := v.DatabaseSpecific.Severity
	for _, a := range v.Affected {
		if word == "" {
			word = a.DatabaseSpecific.Severity
		}
	}
	switch strings.ToUpper(word) {
	case "CRITICAL", "HIGH":
		return High
	case "LOW":
		return Low
	}
	return Medium
}

// Fixed is the lowest fixed version above version for the package, or the
// first fixed version named, or "".
func (v *Vuln) Fixed(name, version string) string {
	var fixes []string
	for _, a := range v.Affected {
		if a.Package.Name != name {
			continue
		}
		for _, r := range a.Ranges {
			for _, event := range r.Events {
				if fixed := event["fixed"]; fixed != "" {
					fixes = append(fixes, fixed)
				}
			}
		}
	}
	slices.SortFunc(fixes, CompareVersions)
	for _, fixed := range fixes {
		if CompareVersions(fixed, version) > 0 {
			return fixed
		}
	}
	if len(fixes) > 0 {
		return fixes[0]
	}
	return ""
}

// Finding is the vulnerability in this package of this repo.
func (v *Vuln) Finding(p Package, repo string) Finding {
	summary := v.Summary
	if summary == "" {
		summary = firstLine(v.Details)
	}
	if summary == "" {
		summary = "a known vulnerability"
	}
	f := Finding{
		ID:       StableID(repo, p.Ecosystem, p.Name+"@"+p.Version, v.ID),
		Severity: v.Severity(),
		Title:    clip(fmt.Sprintf("%s %s in %s: %s", p.Name, p.Version, repo, summary), 200),
		Detail:   clip(fmt.Sprintf("%s (%s). %s", v.ID, strings.Join(append([]string{"https://osv.dev/" + v.ID}, v.Aliases...), ", "), summary), 1000),
		Where:    p.File,
		Repo:     repo,
	}
	if fixed := v.Fixed(p.Name, p.Version); fixed != "" {
		f.Fix = fmt.Sprintf("Update %s to %s or later.", p.Name, fixed)
		f.Detail += " Fixed in " + fixed + "."
	} else {
		f.Fix = fmt.Sprintf("No fixed version of %s yet: read %s and decide whether to replace it.", p.Name, v.ID)
	}
	return f
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return clip(line, 160)
}

// CompareVersions orders versions by their numbers (1.2.10 after 1.2.9), a
// leading v ignored; what isn't a number compares as text.
func CompareVersions(a, b string) int {
	split := func(v string) []string {
		v = strings.TrimPrefix(v, "v")
		return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	}
	as, bs := split(a), split(b)
	for i := 0; i < max(len(as), len(bs)); i++ {
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return an - bn
			}
		case as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	return 0
}

// skipDirs are never searched for lockfiles.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, ".nuxt": true, ".output": true, "target": true, "bin": true, "data": true, ".cache": true}

// maxDepth is how deep below a repo's root lockfiles are looked for.
const maxDepth = 3

// RepoPackages are every package the repo pins, from each go.mod and each
// lockfile (yarn.lock, else package-lock.json, else exact versions in
// package.json) in its root and up to three folders down.
func RepoPackages(root string) []Package {
	var packages []Package
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if path != root && (skipDirs[entry.Name()] || strings.Count(rel, string(filepath.Separator)) >= maxDepth) {
				return filepath.SkipDir
			}
			// A worktree or another repo inside this one is its own repo.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		dir := filepath.Dir(path)
		relDir := filepath.Dir(rel)
		file := func(name string) string { return filepath.Join(relDir, name) }
		switch entry.Name() {
		case "go.mod":
			packages = append(packages, withFile(ParseGoMod(read(path)), file("go.mod"))...)
		case "package.json":
			switch {
			case exists(filepath.Join(dir, "yarn.lock")):
				packages = append(packages, withFile(ParseYarnLock(read(filepath.Join(dir, "yarn.lock"))), file("yarn.lock"))...)
			case exists(filepath.Join(dir, "package-lock.json")):
				packages = append(packages, withFile(ParsePackageLock(read(filepath.Join(dir, "package-lock.json"))), file("package-lock.json"))...)
			default:
				packages = append(packages, withFile(ParsePackageJSON(read(path)), file("package.json"))...)
			}
		}
		return nil
	})
	return packages
}

func read(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func withFile(packages []Package, file string) []Package {
	for i := range packages {
		packages[i].File = filepath.ToSlash(file)
	}
	return packages
}

// ParseGoMod reads a go.mod's requires: in Go 1.17 and later they hold every
// module the build uses, at the version it uses. Replaced modules keep the
// required version; local replacements are not looked up anywhere.
func ParseGoMod(data []byte) []Package {
	var packages []Package
	replaced := map[string]bool{}
	inRequire, inReplace := false, false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "":
			continue
		case line == ")":
			inRequire, inReplace = false, false
			continue
		case line == "require (":
			inRequire = true
			continue
		case line == "replace (":
			inReplace = true
			continue
		}
		fields := strings.Fields(line)
		if !inRequire && !inReplace && len(fields) > 0 {
			switch fields[0] {
			case "require":
				fields = fields[1:]
			case "replace":
				if arrow := slices.Index(fields, "=>"); arrow >= 0 && arrow+1 < len(fields) && isLocal(fields[arrow+1]) {
					replaced[fields[1]] = true
				}
				continue
			default:
				continue
			}
		} else if inReplace {
			if arrow := slices.Index(fields, "=>"); arrow >= 0 && arrow+1 < len(fields) && isLocal(fields[arrow+1]) {
				replaced[fields[0]] = true
			}
			continue
		}
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "v") {
			packages = append(packages, Package{Ecosystem: "Go", Name: fields[0], Version: fields[1]})
		}
	}
	return slices.DeleteFunc(packages, func(p Package) bool { return replaced[p.Name] })
}

func isLocal(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/")
}

var yarnVersion = regexp.MustCompile(`^\s+version:?\s+"?([^"\s]+)"?\s*$`)

// ParseYarnLock reads yarn.lock, classic (v1) or Berry: each entry's
// package name and resolved version. Workspaces and links are left out.
func ParseYarnLock(data []byte) []Package {
	var packages []Package
	seen := map[string]bool{}
	name := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] != ' ' && strings.HasSuffix(line, ":") {
			name = yarnName(strings.TrimSuffix(line, ":"))
			continue
		}
		if name == "" {
			continue
		}
		if m := yarnVersion.FindStringSubmatch(line); m != nil {
			version := m[1]
			if version != "0.0.0-use.local" && !strings.Contains(version, ":") {
				key := name + "@" + version
				if !seen[key] {
					seen[key] = true
					packages = append(packages, Package{Ecosystem: "npm", Name: name, Version: version})
				}
			}
			name = ""
		}
	}
	return packages
}

// yarnName is the package of an entry's header: `"@scope/pkg@^1", "@scope/pkg@npm:^1.2"`.
func yarnName(header string) string {
	first, _, _ := strings.Cut(header, ",")
	first = strings.Trim(strings.TrimSpace(first), `"`)
	at := strings.LastIndex(first, "@")
	if at <= 0 {
		return ""
	}
	name := first[:at]
	if rest := first[at+1:]; strings.HasPrefix(rest, "workspace:") || strings.HasPrefix(rest, "link:") || strings.HasPrefix(rest, "portal:") || strings.HasPrefix(rest, "file:") {
		return ""
	}
	// Berry writes "pkg@npm:1.2.3"; the name is before the first @ after the scope.
	if i := strings.Index(name[1:], "@"); i >= 0 {
		name = name[:i+1]
	}
	return name
}

// ParsePackageLock reads npm's package-lock.json: v2 and v3 `packages`, or
// v1 `dependencies`.
func ParsePackageLock(data []byte) []Package {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return nil
	}
	seen := map[string]bool{}
	var packages []Package
	add := func(name, version string) {
		if name == "" || version == "" || strings.ContainsAny(version, ":/") || seen[name+"@"+version] {
			return
		}
		seen[name+"@"+version] = true
		packages = append(packages, Package{Ecosystem: "npm", Name: name, Version: version})
	}
	if len(lock.Packages) > 0 {
		for path, entry := range lock.Packages {
			i := strings.LastIndex(path, "node_modules/")
			if i < 0 || entry.Link {
				continue
			}
			add(path[i+len("node_modules/"):], entry.Version)
		}
	} else {
		var walk func(map[string]json.RawMessage)
		walk = func(deps map[string]json.RawMessage) {
			for name, raw := range deps {
				var entry struct {
					Version      string                     `json:"version"`
					Dependencies map[string]json.RawMessage `json:"dependencies"`
				}
				if json.Unmarshal(raw, &entry) == nil {
					add(name, entry.Version)
					walk(entry.Dependencies)
				}
			}
		}
		walk(lock.Dependencies)
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Name+packages[i].Version < packages[j].Name+packages[j].Version
	})
	return packages
}

var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// ParsePackageJSON reads the exactly pinned versions of a package.json that
// has no lockfile; ranges can't be known without one.
func ParsePackageJSON(data []byte) []Package {
	var manifest map[string]json.RawMessage
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	var packages []Package
	for _, key := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
		var deps map[string]string
		if json.Unmarshal(manifest[key], &deps) != nil {
			continue
		}
		for name, version := range deps {
			if exactVersion.MatchString(version) {
				packages = append(packages, Package{Ecosystem: "npm", Name: name, Version: version})
			}
		}
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return packages
}
