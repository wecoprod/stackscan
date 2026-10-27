package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cacheTTL = 24 * time.Hour
const eolSoonDays = 180

var httpClient = &http.Client{Timeout: 10 * time.Second}

type Registry struct {
	offline  bool
	cacheDir string
	mu       sync.Mutex
	cycles   map[string][]Cycle // endoflife product -> cycles
	errors   map[string]bool    // sources that could not be reached
}

type Cycle struct {
	LTS     bool
	Cycle   string
	EOL     string // YYYY-MM-DD, "" = no EOL announced
	Latest  string
	Offline bool // from the built-in snapshot
}

func NewRegistry(offline bool) *Registry {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "stackscan")
	os.MkdirAll(dir, 0o755)
	return &Registry{offline: offline, cacheDir: dir, cycles: map[string][]Cycle{}, errors: map[string]bool{}}
}

// fetch returns the body of url, using a 24h on-disk cache.
func (r *Registry) fetch(u string) ([]byte, error) {
	key := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(strings.TrimPrefix(u, "https://"), "_")
	cf := filepath.Join(r.cacheDir, key)
	if st, err := os.Stat(cf); err == nil && (r.offline || time.Since(st.ModTime()) < cacheTTL) {
		return os.ReadFile(cf)
	}
	if r.offline {
		return nil, fmt.Errorf("offline")
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "stackscan/0.1 (+https://github.com/wecoprod/stackscan)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	os.WriteFile(cf, b, 0o644)
	return b, nil
}

func (r *Registry) markError(src string) {
	if r.offline {
		return
	}
	r.mu.Lock()
	r.errors[src] = true
	r.mu.Unlock()
}

// ---------- endoflife.date ----------

func (r *Registry) productCycles(product string) []Cycle {
	r.mu.Lock()
	if c, ok := r.cycles[product]; ok {
		r.mu.Unlock()
		return c
	}
	r.mu.Unlock()

	var out []Cycle
	b, err := r.fetch("https://endoflife.date/api/" + product + ".json")
	if err == nil {
		var raw []map[string]json.RawMessage
		if json.Unmarshal(b, &raw) == nil {
			for _, c := range raw {
				out = append(out, Cycle{Cycle: rawString(c["cycle"]), EOL: rawDate(c["eol"]), Latest: rawString(c["latest"]), LTS: rawLTS(c["lts"])})
			}
		}
	}
	if len(out) == 0 {
		if err != nil {
			r.markError("endoflife.date")
		}
		out = snapshotCycles[product]
	}
	r.mu.Lock()
	r.cycles[product] = out
	r.mu.Unlock()
	return out
}

// lts is either a boolean or the date the cycle became LTS.
func rawLTS(m json.RawMessage) bool {
	var b bool
	if json.Unmarshal(m, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(m, &s) == nil && s != ""
}

func rawString(m json.RawMessage) string {
	var s string
	if json.Unmarshal(m, &s) == nil {
		return s
	}
	var f json.Number
	if json.Unmarshal(m, &f) == nil {
		return f.String()
	}
	return ""
}

// eol is either a date string or a boolean (true = already EOL, date unknown).
func rawDate(m json.RawMessage) string {
	var s string
	if json.Unmarshal(m, &s) == nil {
		return s
	}
	var b bool
	if json.Unmarshal(m, &b) == nil && b {
		return "0001-01-01"
	}
	return ""
}

// matchCycle finds the release cycle a version belongs to. A short version
// ("7" for redis 7.x) resolves to the newest cycle starting with it, which
// is what a floating tag pulls.
func matchCycle(cycles []Cycle, version string) (Cycle, bool) {
	v := numPart(version)
	if v == "" {
		return Cycle{}, false
	}
	parts := strings.Split(v, ".")
	for n := len(parts); n >= 1; n-- {
		cand := strings.Join(parts[:n], ".")
		for _, c := range cycles {
			if c.Cycle == cand {
				return c, true
			}
		}
	}
	var best *Cycle
	for i, c := range cycles {
		if strings.HasPrefix(c.Cycle, v+".") && (best == nil || cmpVersion(c.Cycle, best.Cycle) > 0) {
			best = &cycles[i]
		}
	}
	if best != nil {
		return *best, true
	}
	return Cycle{}, false
}

// newestLTS returns the newest LTS cycle, if the product has any.
func newestLTS(cycles []Cycle) (Cycle, bool) {
	var lts []Cycle
	for _, c := range cycles {
		if c.LTS {
			lts = append(lts, c)
		}
	}
	return newestCycle(lts)
}

func cycleLatest(c Cycle) string {
	if c.Latest != "" {
		return c.Latest
	}
	return c.Cycle
}

func newestCycle(cycles []Cycle) (Cycle, bool) {
	if len(cycles) == 0 {
		return Cycle{}, false
	}
	best := cycles[0]
	for _, c := range cycles[1:] {
		if cmpVersion(c.Cycle, best.Cycle) > 0 {
			best = c
		}
	}
	return best, true
}

// ---------- package registries ----------

func (r *Registry) latestComposer(name string) string {
	b, err := r.fetch("https://repo.packagist.org/p2/" + name + ".json")
	if err != nil {
		r.markError("packagist.org")
		return ""
	}
	var resp struct {
		Packages map[string][]struct {
			Version           string `json:"version"`
			VersionNormalized string `json:"version_normalized"`
		} `json:"packages"`
	}
	if json.Unmarshal(b, &resp) != nil {
		return ""
	}
	best := ""
	for _, v := range resp.Packages[name] {
		if strings.Contains(v.VersionNormalized, "-") || strings.HasPrefix(v.Version, "dev-") {
			continue // RC, beta, dev branches
		}
		if best == "" || cmpVersion(v.Version, best) > 0 {
			best = v.Version
		}
	}
	return best
}

func (r *Registry) latestNpm(name string) string {
	n := name
	if strings.HasPrefix(n, "@") {
		n = strings.Replace(n, "/", "%2F", 1)
	}
	b, err := r.fetch("https://registry.npmjs.org/" + n + "/latest")
	if err != nil {
		r.markError("registry.npmjs.org")
		return ""
	}
	var resp struct{ Version string }
	json.Unmarshal(b, &resp)
	return resp.Version
}

func (r *Registry) latestGo(module string) string {
	esc := regexp.MustCompile(`[A-Z]`).ReplaceAllStringFunc(module, func(s string) string { return "!" + strings.ToLower(s) })
	b, err := r.fetch("https://proxy.golang.org/" + url.PathEscape(esc) + "/@latest")
	if err != nil {
		b, err = r.fetch("https://proxy.golang.org/" + esc + "/@latest")
	}
	if err != nil {
		r.markError("proxy.golang.org")
		return ""
	}
	var resp struct{ Version string }
	json.Unmarshal(b, &resp)
	return resp.Version
}

// frameworkProducts: packages whose release cycles are tracked on endoflife.date.
var frameworkProducts = map[string]string{
	"composer|symfony/framework-bundle": "symfony",
	"composer|laravel/framework":        "laravel",
	"composer|drupal/core":              "drupal",
	"npm|react":                         "react",
	"npm|vue":                           "vue",
	"npm|@angular/core":                 "angular",
	"npm|next":                          "nextjs",
	"npm|nuxt":                          "nuxt",
	"npm|typescript":                    "typescript",
	"npm|electron":                      "electron",
}

// Enrich resolves latest versions and EOL dates for every component.
func (r *Registry) Enrich(pr *Project, all bool) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				r.enrichOne(&pr.Components[i])
			}
		}()
	}
	for i := range pr.Components {
		c := &pr.Components[i]
		if c.Eco == "image" {
			continue // covered by its runtime line
		}
		if !all && !c.Direct && c.Eco != "runtime" {
			continue
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

func (r *Registry) enrichOne(c *Component) {
	product := ""
	if c.Eco == "runtime" {
		product = c.Name
	} else if p, ok := frameworkProducts[c.Eco+"|"+c.Name]; ok {
		product = p
	}
	if product != "" {
		c.Product = product
		cycles := r.productCycles(product)
		if cy, ok := matchCycle(cycles, c.Version); ok {
			c.Cycle = cy.Cycle
			c.EOL = cy.EOL
		}
		if nc, ok := newestCycle(cycles); ok {
			c.Latest = cycleLatest(nc)
			// Products with LTS lines (MySQL, Node.js, Symfony...): the LTS is
			// the production target; the newest release is shown alongside.
			// Only when the LTS is ahead of the version in use: Composer's 2.2
			// LTS, for instance, is a legacy branch, not an upgrade target.
			if lc, ok := newestLTS(cycles); ok && lc.Cycle != nc.Cycle && cmpVersion(lc.Cycle, c.Version) >= 0 {
				c.LatestLTS = cycleLatest(lc)
			}
		}
	}
	if c.Eco != "runtime" {
		switch c.Eco {
		case "composer":
			if l := r.latestComposer(c.Name); l != "" {
				c.Latest = l
			}
		case "npm":
			if l := r.latestNpm(c.Name); l != "" {
				c.Latest = l
			}
		case "go":
			if l := r.latestGo(c.Name); l != "" {
				c.Latest = l
			}
		}
	}
	c.Status = status(c)
}

// isFloor reports a constraint that only sets a minimum (">=8.2", "^8.2"):
// the project may well run a newer, supported version.
func isFloor(v string) bool {
	v = strings.TrimSpace(v)
	return strings.HasPrefix(v, ">") || strings.HasPrefix(v, "^") || strings.Contains(v, "||")
}

func status(c *Component) string {
	if c.Declared && isFloor(c.Version) {
		if c.EOL != "" {
			if d, err := time.Parse("2006-01-02", c.EOL); err == nil && int(time.Until(d).Hours()/24) <= eolSoonDays {
				return "floor"
			}
		}
		return "ok"
	}
	if c.EOL != "" {
		if d, err := time.Parse("2006-01-02", c.EOL); err == nil {
			days := int(time.Until(d).Hours() / 24)
			if days < 0 {
				return "eol"
			}
			if days <= eolSoonDays {
				return "eol-soon"
			}
		}
	}
	if c.Version == "latest" {
		return "ok"
	}
	if c.Latest == "" || numPart(c.Version) == "" {
		return "unknown"
	}
	target := c.Latest
	if c.LatestLTS != "" {
		target = c.LatestLTS
	}
	cur, lat := splitNums(numPart(c.Version)), splitNums(numPart(target))
	if len(cur) == 0 || len(lat) == 0 {
		return "unknown"
	}
	if lat[0] > cur[0] {
		return "major"
	}
	if c.Eco == "runtime" || c.Declared {
		return "ok" // a runtime cycle or a constraint: only majors matter
	}
	if cmpVersion(target, c.Version) > 0 {
		return "minor"
	}
	return "ok"
}

// ---------- versions ----------

var reFirstNum = regexp.MustCompile(`\d+(?:\.\d+)*`)

func numPart(v string) string {
	return reFirstNum.FindString(v)
}

func splitNums(v string) []int {
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func cmpVersion(a, b string) int {
	x, y := splitNums(numPart(a)), splitNums(numPart(b))
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (r *Registry) Unreachable() []string {
	var out []string
	for k := range r.errors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
