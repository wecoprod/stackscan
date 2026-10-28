package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// fakeCloud implements the server side of docs/api.md, checks included, so
// the CLI is tested against the contract rather than against itself.
type fakeCloud struct {
	t      *testing.T
	pub    *[32]byte
	priv   *[32]byte
	keyID  string
	mu     sync.Mutex
	keys   map[string]ed25519.PublicKey
	nonces map[string]bool
	got    []report
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write([]byte(body))
	}
	var env struct {
		V         int    `json:"v"`
		KeyID     string `json:"key_id"`
		ServerKey string `json:"server_key"`
		Sealed    string `json:"sealed"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&env) != nil || env.V != 1 || env.ServerKey != f.keyID {
		reply(400, `{"error":"bad_request"}`)
		return
	}
	sealed, err := base64.StdEncoding.DecodeString(env.Sealed)
	if err != nil {
		reply(400, `{"error":"bad_request"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v1/enroll":
		inner, ok := box.OpenAnonymous(nil, sealed, f.pub, f.priv)
		if !ok {
			reply(400, `{"error":"bad_request"}`)
			return
		}
		var e struct {
			Code      string `json:"code"`
			PublicKey string `json:"public_key"`
		}
		json.Unmarshal(inner, &e)
		if e.Code != "K7Q2-9XMD" {
			reply(401, `{"error":"invalid_code"}`)
			return
		}
		pub, _ := base64.StdEncoding.DecodeString(e.PublicKey)
		f.keys["c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40"] = ed25519.PublicKey(pub)
		reply(201, `{"key_id":"c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40","project":"TousQuali","environment":"prod"}`)

	case "/api/v1/reports":
		inner, ok := box.OpenAnonymous(nil, sealed, f.pub, f.priv)
		if !ok {
			reply(400, `{"error":"bad_request"}`)
			return
		}
		var m struct {
			KeyID   string `json:"key_id"`
			Payload string `json:"payload"`
			TS      int64  `json:"ts"`
			Nonce   string `json:"nonce"`
			Sig     string `json:"sig"`
		}
		json.Unmarshal(inner, &m)
		if m.KeyID != env.KeyID {
			reply(400, `{"error":"bad_request"}`)
			return
		}
		pub, ok := f.keys[env.KeyID]
		payload, _ := base64.StdEncoding.DecodeString(m.Payload)
		sig, _ := base64.StdEncoding.DecodeString(m.Sig)
		sum := sha256.Sum256(payload)
		msg := "stackscan-report-v1\n" + strconv.FormatInt(m.TS, 10) + "\n" + m.Nonce + "\n" + hex.EncodeToString(sum[:])
		skew := time.Now().Unix() - m.TS
		if !ok || !ed25519.Verify(pub, []byte(msg), sig) || skew > 300 || skew < -300 || f.nonces[env.KeyID+m.Nonce] {
			reply(401, `{"error":"unauthorized"}`)
			return
		}
		f.nonces[env.KeyID+m.Nonce] = true
		var rep report
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.DisallowUnknownFields()
		if dec.Decode(&rep) != nil || rep.Schema != 1 || len(rep.Components) == 0 {
			reply(400, `{"error":"bad_request"}`)
			return
		}
		f.got = append(f.got, rep)
		reply(202, `{"status":"accepted"}`)
	default:
		reply(404, `{"error":"not_found"}`)
	}
}

func TestLinkAndPushAgainstContract(t *testing.T) {
	spub, spriv, _ := box.GenerateKey(rand.Reader)
	f := &fakeCloud{t: t, pub: spub, priv: spriv, keyID: "2026-10", keys: map[string]ed25519.PublicKey{}, nonces: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()

	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "compose.yaml"), []byte("services:\n  db:\n    image: postgres:14-alpine\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "composer.json"), []byte(`{"require":{"php":">=8.2","symfony/framework-bundle":"7.4.*"}}`), 0o644)
	t.Setenv("STACKSCAN_URL", srv.URL)
	t.Setenv("STACKSCAN_SERVER_KEY", "2026-10:"+base64.StdEncoding.EncodeToString(spub[:]))
	t.Setenv("STACKSCAN_CONFIG_DIR", t.TempDir())
	t.Setenv("STACKSCAN_KEY", "")

	linkMain([]string{"K7Q2-9XMD", proj})

	// The project file names the key, without any secret.
	pf := readProjectFile(proj)
	id := pf.Environments["prod"]
	if id == "" {
		t.Fatal(".stackscan.json was not written")
	}
	raw, _ := os.ReadFile(filepath.Join(proj, ".stackscan.json"))
	if strings.Contains(string(raw), "ssk1.") {
		t.Fatal(".stackscan.json contains a secret")
	}
	p, _ := keyPath(id)
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file missing or not 0600: %v", err)
	}

	pushMain([]string{"--env", "prod", proj})
	if len(f.got) != 1 {
		t.Fatalf("server accepted %d reports, want 1", len(f.got))
	}
	rep := f.got[0]
	var pg *reportComponent
	for i, c := range rep.Components {
		if c.Eco == "runtime" && c.Name == "postgresql" {
			pg = &rep.Components[i]
		}
	}
	if pg == nil || pg.Version != "14" || pg.Image != "postgres" || pg.Source != "compose.yaml" || !pg.Floating {
		t.Fatalf("postgresql component not reported as expected: %+v", pg)
	}

	// Replaying the exact same request must be refused.
	key, err := resolveKey(proj, "prod")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(rep)
	body, _ := sealReport(key, "2026-10", spub[:], payload, time.Now())
	if s, _, _ := postJSON(srv.URL+"/api/v1/reports", body); s != 202 {
		t.Fatalf("fresh push: got %d", s)
	}
	if s, _, _ := postJSON(srv.URL+"/api/v1/reports", body); s != 401 {
		t.Fatalf("replay: got %d, want 401", s)
	}
	// An old timestamp must be refused.
	old, _ := sealReport(key, "2026-10", spub[:], payload, time.Now().Add(-10*time.Minute))
	if s, _, _ := postJSON(srv.URL+"/api/v1/reports", old); s != 401 {
		t.Fatalf("stale timestamp: got %d, want 401", s)
	}
	// A key_id swapped in clear text no longer matches the signed one inside.
	var envl map[string]any
	json.Unmarshal(body, &envl)
	envl["key_id"] = "someone-else"
	swapped, _ := json.Marshal(envl)
	if s, _, _ := postJSON(srv.URL+"/api/v1/reports", swapped); s != 400 {
		t.Fatalf("swapped key_id: got %d, want 400", s)
	}
	// Nothing readable travels in clear text.
	if bytes.Contains(body, []byte("postgres")) || bytes.Contains(body, []byte("compose.yaml")) {
		t.Fatal("inventory visible in the request body")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	k := signingKey{ID: "c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40", Priv: priv}
	got, err := parseToken(k.token())
	if err != nil || got.ID != k.ID || !bytes.Equal(got.Priv, k.Priv) {
		t.Fatalf("token round trip failed: %v", err)
	}
	for _, bad := range []string{"", "ssk1..x", "ssk2.a.b", "ssk1.a.!!!"} {
		if _, err := parseToken(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCloudURLRequiresHTTPS(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://stackscan.wecoprod.com": true,
		"http://localhost:8000":          true,
		"http://stackscan.wecoprod.com":  false,
		"ftp://x":                        false,
	} {
		t.Setenv("STACKSCAN_URL", u)
		if _, err := cloudURL(); (err == nil) != ok {
			t.Errorf("%s: got err=%v", u, err)
		}
	}
}

// TestWriteServerFixture writes a push request with throwaway keys, for the
// stackscan cloud test suite: STACKSCAN_FIXTURE_OUT=/path/push-v1.json go test -run WriteServerFixture
func TestWriteServerFixture(t *testing.T) {
	out := os.Getenv("STACKSCAN_FIXTURE_OUT")
	if out == "" {
		t.Skip("set STACKSCAN_FIXTURE_OUT to write the fixture")
	}
	spub, spriv, _ := box.GenerateKey(rand.Reader)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := signingKey{ID: "c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40", Priv: priv}
	rep := report{Schema: 1, StackscanVersion: "v0.4.0", ScannedAt: "2026-10-28T15:40:00Z",
		Git: &reportGit{Commit: "99ec32e1a2b3", Branch: "main"},
		Components: []reportComponent{
			{Eco: "runtime", Name: "postgresql", Version: "14", Source: "compose.yaml", Image: "postgres", Direct: true, Floating: true},
			{Eco: "composer", Name: "symfony/framework-bundle", Version: "v7.4.16", Source: "app/composer.lock", Direct: true},
		}}
	payload, _ := json.Marshal(rep)
	body, err := sealReport(key, "test-2026-10", spub[:], payload, time.Unix(1793270400, 0))
	if err != nil {
		t.Fatal(err)
	}
	fx, _ := json.MarshalIndent(map[string]any{
		"comment":            "Throwaway keys, for tests only. Generated by stackscan's TestWriteServerFixture.",
		"server_key_id":      "test-2026-10",
		"server_keypair":     base64.StdEncoding.EncodeToString(append(spriv[:], spub[:]...)),
		"signing_public_key": base64.StdEncoding.EncodeToString(pub),
		"key_id":             key.ID,
		"ts":                 1793270400,
		"request":            string(body),
		"payload":            string(payload),
	}, "", "  ")
	if err := os.WriteFile(out, append(fx, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
