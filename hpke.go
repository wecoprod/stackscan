package main

// HPKE (RFC 9180), base mode, single-shot, with one fixed suite:
// DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-256-GCM.
// Standard library only; checked against the RFC 9180 test vectors in
// hpke_test.go. Used to encrypt everything `stackscan push` sends for the
// public key of stackscan cloud.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const (
	hpkeKEM  = 0x0020 // DHKEM(X25519, HKDF-SHA256)
	hpkeKDF  = 0x0001 // HKDF-SHA256
	hpkeAEAD = 0x0002 // AES-256-GCM
	hpkeNk   = 32
	hpkeNn   = 12
	hpkeEnc  = 32 // size of the encapsulated key
)

func i2osp2(n int) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(n))
	return b
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func labeledExtract(suite, salt []byte, label string, ikm []byte) []byte {
	prk, _ := hkdf.Extract(sha256.New, concat([]byte("HPKE-v1"), suite, []byte(label), ikm), salt)
	return prk
}

func labeledExpand(suite, prk []byte, label string, info []byte, length int) []byte {
	out, _ := hkdf.Expand(sha256.New, prk, string(concat(i2osp2(length), []byte("HPKE-v1"), suite, []byte(label), info)), length)
	return out
}

var (
	kemSuite  = concat([]byte("KEM"), i2osp2(hpkeKEM))
	hpkeSuite = concat([]byte("HPKE"), i2osp2(hpkeKEM), i2osp2(hpkeKDF), i2osp2(hpkeAEAD))
)

// keySchedule derives the AEAD key and nonce (base mode, no PSK).
func keySchedule(sharedSecret, info []byte) (key, nonce []byte) {
	pskIDHash := labeledExtract(hpkeSuite, nil, "psk_id_hash", nil)
	infoHash := labeledExtract(hpkeSuite, nil, "info_hash", info)
	ctx := concat([]byte{0x00}, pskIDHash, infoHash)
	secret := labeledExtract(hpkeSuite, sharedSecret, "secret", nil)
	return labeledExpand(hpkeSuite, secret, "key", ctx, hpkeNk),
		labeledExpand(hpkeSuite, secret, "base_nonce", ctx, hpkeNn)
}

func kemSharedSecret(dh, enc, pkR []byte) []byte {
	prk := labeledExtract(kemSuite, nil, "eae_prk", dh)
	return labeledExpand(kemSuite, prk, "shared_secret", concat(enc, pkR), 32)
}

// hpkeSeal encrypts pt for the X25519 public key pkR. The result is
// enc (32 bytes) followed by the AES-256-GCM ciphertext and tag.
func hpkeSeal(pkR, info, aad, pt []byte) ([]byte, error) {
	skE, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return hpkeSealWith(skE, pkR, info, aad, pt)
}

func hpkeSealWith(skE *ecdh.PrivateKey, pkR, info, aad, pt []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(pkR)
	if err != nil {
		return nil, err
	}
	dh, err := skE.ECDH(pub)
	if err != nil {
		return nil, err
	}
	enc := skE.PublicKey().Bytes()
	key, nonce := keySchedule(kemSharedSecret(dh, enc, pkR), info)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return concat(enc, gcm.Seal(nil, nonce, pt, aad)), nil
}

// hpkeOpen is the receiving side (stackscan cloud); kept here for tests and
// as a reference implementation.
func hpkeOpen(skR *ecdh.PrivateKey, info, aad, sealed []byte) ([]byte, error) {
	if len(sealed) < hpkeEnc+16 {
		return nil, errors.New("hpke: message too short")
	}
	enc, ct := sealed[:hpkeEnc], sealed[hpkeEnc:]
	pkE, err := ecdh.X25519().NewPublicKey(enc)
	if err != nil {
		return nil, err
	}
	dh, err := skR.ECDH(pkE)
	if err != nil {
		return nil, err
	}
	key, nonce := keySchedule(kemSharedSecret(dh, enc, skR.PublicKey().Bytes()), info)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
