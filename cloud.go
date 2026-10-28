package main

// stackscan link / push: send an inventory to stackscan cloud.
// The wire format is described in docs/api.md. In short: the report is
// signed with the environment's Ed25519 key, then encrypted with HPKE for the
// public key of stackscan cloud, built into this binary. Nothing is sent
// unless one of these two commands is run.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultCloudURL = "https://stackscan.wecoprod.com"

// Server encryption keys (X25519 public keys, base64), by identifier. The
// current one encrypts; the next one is listed ahead of a rotation. Set at
// build time (-ldflags "-X main.serverKeyID=... -X main.serverKeyPub=...");
// STACKSCAN_SERVER_KEY="<id>:<base64>" overrides them for a self-hosted or
// development server.
var (
	serverKeyID  = ""
	serverKeyPub = ""
)

const (
	infoEnroll = "stackscan-enroll-v1"
	infoReport = "stackscan-report-v1"
)

var httpCloud = &http.Client{Timeout: 30 * time.Second}

// ---------- configuration ----------

func cloudURL() (string, error) {
	u := os.Getenv("STACKSCAN_URL")
	if u == "" {
		u = defaultCloudURL
	}
	p, err := url.Parse(strings.TrimRight(u, "/"))
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid STACKSCAN_URL %q", u)
	}
	local := p.Hostname() == "localhost" || p.Hostname() == "127.0.0.1" || p.Hostname() == "::1"
	if p.Scheme != "https" && !(p.Scheme == "http" && local) {
		return "", fmt.Errorf("STACKSCAN_URL must use https (plain http is only allowed for localhost)")
	}
	return p.String(), nil
}

func serverKey() (id string, pub []byte, err error) {
	id, b64 := serverKeyID, serverKeyPub
	if v := os.Getenv("STACKSCAN_SERVER_KEY"); v != "" {
		var ok bool
		id, b64, ok = strings.Cut(v, ":")
		if !ok {
			return "", nil, errors.New(`STACKSCAN_SERVER_KEY must be "<id>:<base64 X25519 public key>"`)
		}
	}
	if id == "" || b64 == "" {
		return "", nil, errors.New("this build has no stackscan cloud key; set STACKSCAN_SERVER_KEY")
	}
	pub, err = base64.StdEncoding.DecodeString(b64)
	if err == nil {
		_, err = ecdh.X25519().NewPublicKey(pub)
	}
	if err != nil {
		return "", nil, fmt.Errorf("invalid server key: %v", err)
	}
	return id, pub, nil
}

func configDir() (string, error) {
	if d := os.Getenv("STACKSCAN_CONFIG_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "stackscan"), nil
}

// A signing key travels as one token: "ssk1.<key_id>.<base64url seed>". It
// is what `link` stores (mode 0600) and what STACKSCAN_KEY holds in CI.
type signingKey struct {
	ID   string
	Priv ed25519.PrivateKey
}

func (k signingKey) token() string {
	return "ssk1." + k.ID + "." + base64.RawURLEncoding.EncodeToString(k.Priv.Seed())
}

func parseToken(s string) (signingKey, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 || parts[0] != "ssk1" || parts[1] == "" {
		return signingKey{}, errors.New("invalid stackscan key (expected ssk1.<id>.<secret>)")
	}
	seed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(seed) != ed25519.SeedSize {
		return signingKey{}, errors.New("invalid stackscan key secret")
	}
	return signingKey{ID: parts[1], Priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// .stackscan.json, at the root of the project: which key each environment
// uses. Holds key identifiers only, no secret: it can be committed.
type projectFile struct {
	Environments map[string]string `json:"environments"`
}

func projectRoot(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	abs, _ := filepath.Abs(dir)
	return abs
}

func readProjectFile(root string) projectFile {
	var pf projectFile
	if b, err := os.ReadFile(filepath.Join(root, ".stackscan.json")); err == nil {
		json.Unmarshal(b, &pf)
	}
	if pf.Environments == nil {
		pf.Environments = map[string]string{}
	}
	return pf
}

func keyPath(id string) (string, error) {
	if strings.ContainsAny(id, `/\.`) {
		return "", errors.New("invalid key id")
	}
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "keys", id+".key"), nil
}

// ---------- link ----------

func linkMain(args []string) {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: stackscan link <code> [dir]\n\nConnects this folder to a stackscan cloud environment, with the one-time\ncode shown in the web interface. The signing key is created here and never\nleaves this machine.\n")
	}
	fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	code := fs.Arg(0)
	dir := "."
	if fs.NArg() > 1 {
		dir = fs.Arg(1)
	}

	base, err := cloudURL()
	check(err)
	skID, skPub, err := serverKey()
	check(err)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	check(err)
	nonce := make([]byte, 16)
	rand.Read(nonce)
	inner, _ := json.Marshal(map[string]any{
		"code":        code,
		"public_key":  base64.StdEncoding.EncodeToString(pub),
		"ts":          time.Now().Unix(),
		"nonce":       base64.StdEncoding.EncodeToString(nonce),
		"cli_version": cliVersion(),
	})
	sealed, err := hpkeSeal(skPub, []byte(infoEnroll), []byte("enroll\n"+skID), inner)
	check(err)
	body, _ := json.Marshal(map[string]any{"v": 1, "server_key": skID, "sealed": base64.StdEncoding.EncodeToString(sealed)})

	status, resp, err := postJSON(base+"/api/v1/enroll", body)
	check(err)
	if status != http.StatusCreated {
		fail("link failed: %s", describeError(status, resp))
	}
	var r struct {
		KeyID       string `json:"key_id"`
		Project     string `json:"project"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || r.KeyID == "" || r.Environment == "" {
		fail("link failed: unexpected response from %s", base)
	}
	key := signingKey{ID: r.KeyID, Priv: priv}

	p, err := keyPath(key.ID)
	check(err)
	check(os.MkdirAll(filepath.Dir(p), 0o700))
	check(os.WriteFile(p, []byte(key.token()+"\n"), 0o600))

	root := projectRoot(dir)
	pf := readProjectFile(root)
	pf.Environments[r.Environment] = key.ID
	b, _ := json.MarshalIndent(pf, "", "  ")
	check(os.WriteFile(filepath.Join(root, ".stackscan.json"), append(b, '\n'), 0o644))

	fmt.Printf("✓ Linked to %s · %s\n", r.Project, r.Environment)
	fmt.Printf("  key saved in %s (keep it private)\n", p)
	fmt.Printf("  %s updated (no secret, can be committed)\n", filepath.Join(root, ".stackscan.json"))
	fmt.Printf("\nNext: stackscan push --env %s\n", r.Environment)
	fmt.Println("For CI, store this in a masked variable named STACKSCAN_KEY:")
	fmt.Println("  " + key.token())
}

// ---------- push ----------

// reportComponent is one entry of docs/report.schema.json.
type reportComponent struct {
	Eco      string `json:"eco"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Source   string `json:"source"`
	Image    string `json:"image,omitempty"`
	Direct   bool   `json:"direct,omitempty"`
	Dev      bool   `json:"dev,omitempty"`
	Declared bool   `json:"declared,omitempty"`
	Floating bool   `json:"floating,omitempty"`
}

type report struct {
	Schema           int               `json:"schema"`
	StackscanVersion string            `json:"stackscan_version"`
	ScannedAt        string            `json:"scanned_at"`
	Git              *reportGit        `json:"git,omitempty"`
	Components       []reportComponent `json:"components"`
}

type reportGit struct {
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func buildReport(pr *Project, redact bool) report {
	rep := report{Schema: 1, StackscanVersion: cliVersion(), ScannedAt: time.Now().UTC().Format(time.RFC3339)}
	git := func(a ...string) string {
		out, _ := exec.Command("git", append([]string{"-C", pr.Path}, a...)...).Output()
		return strings.TrimSpace(string(out))
	}
	if c := git("rev-parse", "--short=12", "HEAD"); c != "" {
		rep.Git = &reportGit{Commit: c, Branch: git("rev-parse", "--abbrev-ref", "HEAD")}
	}
	seen := map[string]bool{}
	for _, c := range pr.Components {
		file, detail, _ := strings.Cut(c.Source, " (")
		detail = strings.TrimSuffix(detail, ")")
		rc := reportComponent{
			Eco: c.Eco, Name: c.Name, Version: c.Version, Source: file,
			Direct: c.Direct, Dev: c.Dev, Declared: c.Declared, Floating: c.Floating,
		}
		if c.Eco == "runtime" && detail != "" && !strings.Contains(detail, ".") && detail != "engines" {
			rc.Image = detail // runtime deduced from a Docker image
		}
		if redact {
			h := sha256.Sum256([]byte(rc.Source))
			rc.Source = "sha256:" + hex.EncodeToString(h[:8])
		}
		k := rc.Eco + "|" + rc.Name + "|" + rc.Version + "|" + rc.Source + "|" + rc.Image
		if !seen[k] {
			seen[k] = true
			rep.Components = append(rep.Components, rc)
		}
	}
	sort.SliceStable(rep.Components, func(i, j int) bool {
		a, b := rep.Components[i], rep.Components[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Eco != b.Eco {
			return a.Eco < b.Eco
		}
		return a.Name < b.Name
	})
	return rep
}

// sealReport builds the request body of POST /api/v1/reports (docs/api.md).
func sealReport(key signingKey, skID string, skPub, payload []byte, now time.Time) ([]byte, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	sum := sha256.Sum256(payload)
	msg := "stackscan-report-v1\n" + ts + "\n" + nonceB64 + "\n" + hex.EncodeToString(sum[:])
	inner, _ := json.Marshal(map[string]any{
		"payload": base64.StdEncoding.EncodeToString(payload),
		"ts":      now.Unix(),
		"nonce":   nonceB64,
		"sig":     base64.StdEncoding.EncodeToString(ed25519.Sign(key.Priv, []byte(msg))),
	})
	sealed, err := hpkeSeal(skPub, []byte(infoReport), []byte(key.ID+"\n"+skID), inner)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"v": 1, "key_id": key.ID, "server_key": skID,
		"sealed": base64.StdEncoding.EncodeToString(sealed),
	})
}

func pushMain(args []string) {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	env := fs.String("env", "", "environment to push to, as linked in .stackscan.json (not needed with STACKSCAN_KEY)")
	dry := fs.Bool("dry-run", false, "print the report that would be sent, unencrypted, and send nothing")
	redact := fs.Bool("redact-paths", false, "replace manifest paths with hashes")
	depth := fs.Int("depth", 4, "maximum directory depth to search for manifests")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: stackscan push [--env <env>] [--dry-run] [dir]\n\nScans the project and sends its inventory to stackscan cloud, signed and\nend-to-end encrypted. See docs/api.md for exactly what is sent.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}

	pr, err := scanProject(dir, *depth)
	check(err)
	if len(pr.Components) == 0 {
		fail("push: nothing to report in %s", pr.Path)
	}
	payload, _ := json.Marshal(buildReport(pr, *redact))
	if *dry {
		var pretty bytes.Buffer
		json.Indent(&pretty, payload, "", "  ")
		fmt.Println(pretty.String())
		fmt.Fprintf(os.Stderr, "dry run: %d components, %d bytes, nothing sent\n", len(pr.Components), len(payload))
		return
	}

	key, err := resolveKey(projectRoot(dir), *env)
	check(err)
	base, err := cloudURL()
	check(err)
	skID, skPub, err := serverKey()
	check(err)
	body, err := sealReport(key, skID, skPub, payload, time.Now())
	check(err)

	status, resp, err := postJSON(base+"/api/v1/reports", body)
	check(err)
	if status != http.StatusAccepted {
		fail("push failed: %s", describeError(status, resp))
	}
	fmt.Printf("✓ Pushed %d components to %s\n", len(pr.Components), base)
}

func resolveKey(root, env string) (signingKey, error) {
	if t := os.Getenv("STACKSCAN_KEY"); t != "" {
		return parseToken(t)
	}
	pf := readProjectFile(root)
	id := pf.Environments[env]
	if env == "" {
		if len(pf.Environments) != 1 {
			var names []string
			for n := range pf.Environments {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) == 0 {
				return signingKey{}, errors.New("no environment linked here: run stackscan link <code> first")
			}
			return signingKey{}, fmt.Errorf("several environments are linked (%s): use --env", strings.Join(names, ", "))
		}
		for _, v := range pf.Environments {
			id = v
		}
	}
	if id == "" {
		return signingKey{}, fmt.Errorf("environment %q is not linked here: run stackscan link <code>", env)
	}
	p, err := keyPath(id)
	if err != nil {
		return signingKey{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return signingKey{}, fmt.Errorf("no key for this environment on this machine (%s): link it here, or set STACKSCAN_KEY", p)
	}
	return parseToken(string(b))
}

// ---------- HTTP ----------

func postJSON(u string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "stackscan/"+cliVersion())
	resp, err := httpCloud.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func describeError(status int, body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(body, &e)
	switch {
	case status == http.StatusUnauthorized && e.Error == "invalid_code":
		return "unknown, expired or already used code"
	case status == http.StatusUnauthorized:
		return "rejected (revoked key, wrong clock, or invalid signature)"
	case status == http.StatusTooManyRequests:
		return "too many requests, try again later"
	case status == http.StatusRequestEntityTooLarge:
		return "report too large"
	case e.Error != "":
		return fmt.Sprintf("HTTP %d (%s)", status, e.Error)
	}
	return fmt.Sprintf("HTTP %d", status)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}
