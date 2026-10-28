package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// RFC 9180 test vector: mode_base, DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
// AES-256-GCM (from the CFRG test-vectors.json).
type hpkeVector struct {
	Info         string `json:"info"`
	SkEm         string `json:"skEm"`
	PkEm         string `json:"pkEm"`
	SkRm         string `json:"skRm"`
	PkRm         string `json:"pkRm"`
	Enc          string `json:"enc"`
	SharedSecret string `json:"shared_secret"`
	Key          string `json:"key"`
	BaseNonce    string `json:"base_nonce"`
	Encryptions  []struct {
		AAD   string `json:"aad"`
		PT    string `json:"pt"`
		Nonce string `json:"nonce"`
		CT    string `json:"ct"`
	} `json:"encryptions"`
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHPKEVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/hpke-x25519-sha256-aes256gcm.json")
	if err != nil {
		t.Fatal(err)
	}
	var v hpkeVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	skE, err := ecdh.X25519().NewPrivateKey(unhex(t, v.SkEm))
	if err != nil {
		t.Fatal(err)
	}
	skR, err := ecdh.X25519().NewPrivateKey(unhex(t, v.SkRm))
	if err != nil {
		t.Fatal(err)
	}
	pkR := unhex(t, v.PkRm)
	info := unhex(t, v.Info)

	// Intermediate values.
	dh, _ := skE.ECDH(skR.PublicKey())
	ss := kemSharedSecret(dh, unhex(t, v.Enc), pkR)
	if !bytes.Equal(ss, unhex(t, v.SharedSecret)) {
		t.Fatalf("shared_secret mismatch")
	}
	key, nonce := keySchedule(ss, info)
	if !bytes.Equal(key, unhex(t, v.Key)) || !bytes.Equal(nonce, unhex(t, v.BaseNonce)) {
		t.Fatalf("key schedule mismatch")
	}

	// First encryption (sequence 0) is what single-shot Seal produces.
	e := v.Encryptions[0]
	sealed, err := hpkeSealWith(skE, pkR, info, unhex(t, e.AAD), unhex(t, e.PT))
	if err != nil {
		t.Fatal(err)
	}
	want := append(unhex(t, v.Enc), unhex(t, e.CT)...)
	if !bytes.Equal(sealed, want) {
		t.Fatalf("seal mismatch")
	}
	pt, err := hpkeOpen(skR, info, unhex(t, e.AAD), sealed)
	if err != nil || !bytes.Equal(pt, unhex(t, e.PT)) {
		t.Fatalf("open failed: %v", err)
	}
}

func TestHPKERoundTripAndTamper(t *testing.T) {
	skR, _ := ecdh.X25519().GenerateKey(rand.Reader)
	sealed, err := hpkeSeal(skR.PublicKey().Bytes(), []byte("stackscan-v1"), []byte("aad"), []byte("inventory"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := hpkeOpen(skR, []byte("stackscan-v1"), []byte("aad"), sealed); err != nil || string(pt) != "inventory" {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := hpkeOpen(skR, []byte("stackscan-v1"), []byte("other aad"), sealed); err == nil {
		t.Fatal("wrong aad accepted")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := hpkeOpen(skR, []byte("stackscan-v1"), []byte("aad"), sealed); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
