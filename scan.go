package main

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Component is one versioned thing found in a project: a composer/npm/go
// package, a Docker image, or a runtime (php, nodejs, postgresql...).
type Component struct {
	Eco      string `json:"eco"`     // composer | npm | go | image | runtime
	Name     string `json:"name"`    // package name, image repo, or endoflife product
	Version  string `json:"version"` // as found (installed version, tag, constraint)
	Direct   bool   `json:"direct"`  // declared directly by the project
	Dev      bool   `json:"dev"`
	Source   string `json:"source"`   // file, relative to the project
	Floating bool   `json:"floating"` // tag/constraint that moves without a commit
	Declared bool   `json:"declared"` // constraint only (no lock file)

	// Filled by the enrich step.
	Latest    string `json:"latest,omitempty"`
	LatestLTS string `json:"latestLts,omitempty"` // newest LTS when it differs from Latest
	EOL       string `json:"eol,omitempty"`       // YYYY-MM-DD, "" if unknown or none
	Cycle     string `json:"cycle,omitempty"`
	Status    string `json:"status,omitempty"` // eol | eol-soon | major | minor | ok | unknown
	Product   string `json:"product,omitempty"`
}

type Project struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Remote     string      `json:"remote,omitempty"`
	LastCommit string      `json:"lastCommit,omitempty"`
	Components []Component `json:"components"`
}

var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, "var": true,
	"cache": true, ".cache": true, "dist": true, "build": true, ".next": true,
	".nuxt": true, ".astro": true, ".idea": true, ".vscode": true, "public": true,
}

func isManifest(name string) bool {
	switch name {
	case "composer.json", "composer.lock", "package.json", "package-lock.json",
		"go.mod", ".nvmrc", ".php-version", ".tool-versions":
		return true
	}
	if name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile") {
		return true
	}
	if (strings.HasPrefix(name, "compose") || strings.HasPrefix(name, "docker-compose")) &&
		(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
		return true
	}
	return false
}

// scanProject collects every manifest under dir (skipping vendor,
// node_modules, nested git repositories...) and parses them as one project.
func scanProject(dir string, maxDepth int) (*Project, error) {
	dir, _ = filepath.Abs(dir)
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == dir {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if skipDirs[d.Name()] || strings.Count(rel, string(os.PathSeparator))+1 > maxDepth {
				return filepath.SkipDir
			}
			// A nested repository or worktree is another project.
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if isManifest(d.Name()) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pr := &Project{Name: filepath.Base(dir), Path: dir}
	parseProject(pr, files)
	gitInfo(pr)
	return pr, nil
}

func parseProject(pr *Project, files []string) {
	sort.Strings(files)
	seen := map[string]bool{}
	add := func(c Component) {
		key := c.Eco + "|" + c.Name + "|" + c.Version + "|" + c.Source
		if seen[key] {
			return
		}
		seen[key] = true
		pr.Components = append(pr.Components, c)
	}
	rel := func(f string) string {
		r, _ := filepath.Rel(pr.Path, f)
		return r
	}
	hasLock := map[string]bool{}
	for _, f := range files {
		hasLock[f] = true
	}
	for _, f := range files {
		base := filepath.Base(f)
		dir := filepath.Dir(f)
		switch {
		case base == "composer.json":
			parseComposer(f, hasLock[filepath.Join(dir, "composer.lock")], rel(f), rel(filepath.Join(dir, "composer.lock")), add)
		case base == "package.json":
			parseNpm(f, hasLock[filepath.Join(dir, "package-lock.json")], rel(f), rel(filepath.Join(dir, "package-lock.json")), add)
		case base == "go.mod":
			parseGoMod(f, rel(f), add)
		case base == ".nvmrc":
			if b, err := os.ReadFile(f); err == nil {
				v := strings.TrimPrefix(strings.TrimSpace(string(b)), "v")
				add(Component{Eco: "runtime", Name: "nodejs", Version: v, Direct: true, Source: rel(f), Declared: true})
			}
		case base == ".php-version":
			if b, err := os.ReadFile(f); err == nil {
				add(Component{Eco: "runtime", Name: "php", Version: strings.TrimSpace(string(b)), Direct: true, Source: rel(f), Declared: true})
			}
		case base == ".tool-versions":
			parseToolVersions(f, rel(f), add)
		case strings.Contains(base, "ockerfile"):
			parseDockerfile(f, rel(f), add)
		case strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml"):
			parseCompose(f, rel(f), add)
		}
	}
}

// ---------- composer ----------

type composerJSON struct {
	Require    map[string]string `json:"require"`
	RequireDev map[string]string `json:"require-dev"`
	Config     struct {
		Platform map[string]string `json:"platform"`
	} `json:"config"`
}

type composerLock struct {
	Packages    []struct{ Name, Version string } `json:"packages"`
	PackagesDev []struct{ Name, Version string } `json:"packages-dev"`
}

func parseComposer(f string, lockExists bool, src, lockSrc string, add func(Component)) {
	var cj composerJSON
	if !readJSON(f, &cj) {
		return
	}
	if php, ok := cj.Config.Platform["php"]; ok {
		add(Component{Eco: "runtime", Name: "php", Version: php, Direct: true, Source: src + " (config.platform)", Declared: true})
	} else if php, ok := cj.Require["php"]; ok {
		add(Component{Eco: "runtime", Name: "php", Version: php, Direct: true, Source: src, Declared: true})
	}
	direct := map[string]bool{}
	for k := range cj.Require {
		direct[k] = true
	}
	for k := range cj.RequireDev {
		direct[k] = true
	}
	isPlatform := func(n string) bool {
		return n == "php" || strings.HasPrefix(n, "ext-") || strings.HasPrefix(n, "lib-") || !strings.Contains(n, "/")
	}
	if lockExists {
		var cl composerLock
		lf := filepath.Join(filepath.Dir(f), "composer.lock")
		if readJSON(lf, &cl) {
			for _, p := range cl.Packages {
				add(Component{Eco: "composer", Name: p.Name, Version: p.Version, Direct: direct[p.Name], Source: lockSrc})
			}
			for _, p := range cl.PackagesDev {
				add(Component{Eco: "composer", Name: p.Name, Version: p.Version, Direct: direct[p.Name], Dev: true, Source: lockSrc})
			}
			return
		}
	}
	for n, v := range cj.Require {
		if !isPlatform(n) {
			add(Component{Eco: "composer", Name: n, Version: v, Direct: true, Source: src, Declared: true})
		}
	}
	for n, v := range cj.RequireDev {
		if !isPlatform(n) {
			add(Component{Eco: "composer", Name: n, Version: v, Direct: true, Dev: true, Source: src, Declared: true})
		}
	}
}

// ---------- npm ----------

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
}

type packageLock struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Version string `json:"version"`
		Dev     bool   `json:"dev"`
	} `json:"packages"`
	Dependencies map[string]struct {
		Version string `json:"version"`
		Dev     bool   `json:"dev"`
	} `json:"dependencies"`
}

func parseNpm(f string, lockExists bool, src, lockSrc string, add func(Component)) {
	var pj packageJSON
	if !readJSON(f, &pj) {
		return
	}
	if n, ok := pj.Engines["node"]; ok {
		add(Component{Eco: "runtime", Name: "nodejs", Version: n, Direct: true, Source: src + " (engines)", Declared: true})
	}
	direct := map[string]bool{}
	dev := map[string]bool{}
	for k := range pj.Dependencies {
		direct[k] = true
	}
	for k := range pj.DevDependencies {
		direct[k] = true
		dev[k] = true
	}
	if lockExists {
		var pl packageLock
		lf := filepath.Join(filepath.Dir(f), "package-lock.json")
		if readJSON(lf, &pl) {
			if len(pl.Packages) > 0 {
				for k, v := range pl.Packages {
					if !strings.HasPrefix(k, "node_modules/") || strings.Contains(strings.TrimPrefix(k, "node_modules/"), "/node_modules/") {
						continue
					}
					name := strings.TrimPrefix(k, "node_modules/")
					add(Component{Eco: "npm", Name: name, Version: v.Version, Direct: direct[name], Dev: v.Dev, Source: lockSrc})
				}
			} else {
				for name, v := range pl.Dependencies {
					add(Component{Eco: "npm", Name: name, Version: v.Version, Direct: direct[name], Dev: v.Dev, Source: lockSrc})
				}
			}
			return
		}
	}
	for n, v := range pj.Dependencies {
		add(Component{Eco: "npm", Name: n, Version: v, Direct: true, Source: src, Declared: true})
	}
	for n, v := range pj.DevDependencies {
		add(Component{Eco: "npm", Name: n, Version: v, Direct: true, Dev: true, Source: src, Declared: true})
	}
}

// ---------- go ----------

func parseGoMod(f, src string, add func(Component)) {
	fh, err := os.Open(f)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	inReq := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "go "):
			add(Component{Eco: "runtime", Name: "go", Version: strings.TrimSpace(line[3:]), Direct: true, Source: src, Declared: true})
		case line == "require (":
			inReq = true
		case inReq && line == ")":
			inReq = false
		case inReq || strings.HasPrefix(line, "require "):
			l := strings.TrimPrefix(line, "require ")
			parts := strings.Fields(l)
			if len(parts) >= 2 && !strings.HasPrefix(parts[0], "//") {
				add(Component{Eco: "go", Name: parts[0], Version: parts[1], Direct: !strings.Contains(l, "// indirect"), Source: src})
			}
		}
	}
}

func parseToolVersions(f, src string, add func(Component)) {
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	names := map[string]string{"nodejs": "nodejs", "node": "nodejs", "php": "php", "python": "python", "golang": "go", "postgres": "postgresql", "ruby": "ruby"}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			if p, ok := names[parts[0]]; ok {
				add(Component{Eco: "runtime", Name: p, Version: parts[1], Direct: true, Source: src, Declared: true})
			}
		}
	}
}

// ---------- Docker ----------

var (
	reFrom  = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	reArg   = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)=(\S+)`)
	reImage = regexp.MustCompile(`^\s*image:\s*['"]?([^'"#\s]+)`)
	reVar   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
)

func expand(s string, vars map[string]string) (string, bool) {
	ok := true
	out := reVar.ReplaceAllStringFunc(s, func(m string) string {
		sub := reVar.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[3]
		}
		if v, found := vars[name]; found {
			return v
		}
		if strings.Contains(m, "-") && sub[1] != "" {
			return sub[2]
		}
		ok = false
		return m
	})
	return out, ok
}

func parseDockerfile(f, src string, add func(Component)) {
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	stages := map[string]bool{}
	vars := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if m := reArg.FindStringSubmatch(line); m != nil {
			vars[m[1]] = strings.Trim(m[2], `"'`)
			continue
		}
		m := reFrom.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := m[1]
		if m[2] != "" {
			stages[strings.ToLower(m[2])] = true
		}
		if stages[strings.ToLower(ref)] || strings.EqualFold(ref, "scratch") {
			continue
		}
		ref, _ = expand(ref, vars)
		addImage(ref, src, add)
	}
}

func parseCompose(f, src string, add func(Component)) {
	fh, err := os.Open(f)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		m := reImage.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		ref, _ := expand(m[1], nil)
		addImage(ref, src, add)
	}
}

// imageProducts maps an image repository to its endoflife.date product.
var imageProducts = map[string]string{
	"php": "php", "dunglas/frankenphp": "php", "node": "nodejs", "postgres": "postgresql",
	"postgis/postgis": "postgresql", "mysql": "mysql", "mariadb": "mariadb", "redis": "redis",
	"valkey/valkey": "valkey", "python": "python", "nginx": "nginx", "caddy": "caddy",
	"debian": "debian", "alpine": "alpine", "ubuntu": "ubuntu", "wordpress": "wordpress",
	"ghost": "ghost", "rabbitmq": "rabbitmq", "golang": "go", "composer": "composer",
	"httpd": "apache-http-server", "mongo": "mongodb", "elasticsearch": "elasticsearch",
	"traefik": "traefik", "memcached": "memcached", "bitnami/redis": "redis",
	"bitnami/postgresql": "postgresql", "clamav/clamav": "clamav", "minio/minio": "minio",
}

var reNum = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)`)
var rePhpInTag = regexp.MustCompile(`php(\d+(?:\.\d+)*)`)

func splitImage(ref string) (repo, tag string) {
	ref = strings.SplitN(ref, "@", 2)[0]
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		repo, tag = ref[:colon], ref[colon+1:]
	} else {
		repo = ref
	}
	repo = strings.TrimPrefix(repo, "docker.io/")
	repo = strings.TrimPrefix(repo, "library/")
	return
}

func addImage(ref, src string, add func(Component)) {
	if ref == "" || strings.Contains(ref, "$") {
		// Own image whose name/tag is injected at deploy time: nothing to compare.
		return
	}
	repo, tag := splitImage(ref)
	floating := tag == "" || tag == "latest"
	ver := tag
	if tag == "" {
		ver = "latest"
	}
	add(Component{Eco: "image", Name: repo, Version: ver, Direct: true, Source: src, Floating: floating || !hasPatch(tag)})

	product, known := imageProducts[repo]
	if !known {
		return
	}
	var v string
	if repo == "dunglas/frankenphp" {
		if m := rePhpInTag.FindStringSubmatch(tag); m != nil {
			v = m[1]
		}
	} else if m := reNum.FindStringSubmatch(tag); m != nil {
		v = m[1]
	}
	if v == "" {
		v = "latest"
	}
	add(Component{Eco: "runtime", Name: product, Version: v, Direct: true, Source: src + " (" + repo + ")", Floating: v == "latest" || !hasPatch(v)})
}

func hasPatch(v string) bool {
	m := reNum.FindStringSubmatch(v)
	return m != nil && strings.Count(m[1], ".") >= 2
}

func readJSON(f string, v any) bool {
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func gitInfo(pr *Project) {
	if out, err := exec.Command("git", "-C", pr.Path, "remote", "get-url", "origin").Output(); err == nil {
		r := strings.TrimSpace(string(out))
		pr.Remote = regexp.MustCompile(`https://[^@/]+@`).ReplaceAllString(r, "https://")
	}
	if out, err := exec.Command("git", "-C", pr.Path, "log", "-1", "--format=%cs").Output(); err == nil {
		pr.LastCommit = strings.TrimSpace(string(out))
	}
}
