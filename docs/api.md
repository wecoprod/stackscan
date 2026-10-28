# stackscan cloud API — v1

This document is the contract between the open-source `stackscan` CLI and
stackscan cloud. It is public on purpose: you can check exactly what leaves
your machine. `stackscan push --dry-run` prints the same payload, unencrypted,
without sending it.

Nothing is ever sent unless you run `stackscan link` and `stackscan push`.

## Base URL

`https://stackscan.wecoprod.com/api/v1/`

The CLI uses this address by default; `STACKSCAN_URL` overrides it.

## Principles

- **Push only.** stackscan cloud never connects to your machines.
- **Minimal data.** Only what the [report schema](report.schema.json) lists:
  ecosystem, package name, version, manifest path and a few flags. No file
  contents, environment variables, host names or IP addresses.
- **End-to-end encryption.** Every request body is encrypted with the public
  key of stackscan cloud, built into the CLI binary. Anything between the CLI
  and the application (network, proxies doing TLS inspection, the reverse
  proxy, access logs) only sees an opaque blob.
- **Signed.** Every report is signed with an Ed25519 key that never leaves the
  machine that pushes. The server only stores public keys.
- **HTTPS only**, with certificate verification that cannot be turned off.

## Keys

| Key | Type | Where it lives |
| --- | --- | --- |
| Server encryption key | X25519 key pair | private key: stackscan cloud only; public key: built into the CLI |
| Environment signing key | Ed25519 key pair | private key: the machine or CI that pushes; public key: stackscan cloud |

The CLI embeds the current server public key and the next one, each with an
identifier (`server_key`), so the server key can be rotated without breaking
older CLIs.

One signing key belongs to exactly one project environment (e.g. *Birdie /
prod*). It can only write reports for that environment, never read anything,
and can be revoked at any time from the web interface.

## Encoding

- Binary values (keys, nonces, signatures, sealed messages) are standard
  base64 with padding (RFC 4648 §4).
- `ts` is a Unix timestamp in seconds.
- `nonce` is 16 random bytes.

### Encryption: libsodium sealed box

`sealed` is a libsodium **sealed box** (`crypto_box_seal`: ephemeral X25519
key, XSalsa20-Poly1305) for the server public key named by `server_key`.

- CLI: `golang.org/x/crypto/nacl/box.SealAnonymous`.
- Server: Symfony 8.2 `SealedBoxKms` (`symfony/key-management`), with
  `server_key` as the key id: `$kms->decrypt(new Ciphertext($sealed, $serverKey))`;
  or plain `sodium_crypto_box_seal_open`.

A sealed box has no associated data, so the clear-text `key_id` of a report is
repeated inside the encrypted content and the server checks that both match.

## 1. Enroll an environment — `POST /api/v1/enroll`

In the web interface, *Connect an environment* shows a one-time code, valid 15
minutes. `stackscan link <code>` then generates the Ed25519 key pair locally and
sends the public key.

Request:

```json
{
  "v": 1,
  "server_key": "2026-10",
  "sealed": "<base64 sealed box>"
}
```

Plaintext inside `sealed`:

```json
{
  "code": "K7Q2-9XMD",
  "public_key": "<base64, 32 bytes Ed25519>",
  "ts": 1793270400,
  "nonce": "<base64, 16 bytes>",
  "cli_version": "v0.4.0"
}
```

Responses:

| Status | Body | Meaning |
| --- | --- | --- |
| `201` | `{"key_id": "<uuid>", "project": "Birdie", "environment": "prod"}` | linked; the CLI stores `key_id` with the private key |
| `400` | `{"error": "bad_request"}` | malformed request or unknown `v` / `server_key` |
| `401` | `{"error": "invalid_code"}` | unknown, expired or already used code |
| `429` | `{"error": "rate_limited"}` + `Retry-After` | too many attempts |

The response travels over TLS only; project and environment names are shown so
the user can confirm the link.

## 2. Push a report — `POST /api/v1/reports`

Request (the only clear-text field is an opaque key identifier):

```json
{
  "v": 1,
  "key_id": "c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40",
  "server_key": "2026-10",
  "sealed": "<base64 sealed box>"
}
```

Plaintext inside `sealed`:

```json
{
  "key_id": "c91e5b7a-0d8f-4c1e-9a3b-6f2d1e8a7b40",
  "payload": "<base64 of the report JSON bytes>",
  "ts": 1793270400,
  "nonce": "<base64, 16 bytes>",
  "sig": "<base64, 64 bytes Ed25519>"
}
```

The report travels as base64 bytes so that the signed bytes are exactly the
received bytes, with no JSON canonicalisation.

### Signature

```
message = "stackscan-report-v1" + "\n" + ts + "\n" + nonce + "\n" + hex(sha256(payload bytes))
sig     = Ed25519_sign(environment private key, message)
```

`ts` is written in decimal and `nonce` in base64, exactly as sent.

### Server checks, in order

1. `v` and `server_key` are known; body ≤ 4 MiB.
2. `sealed` opens with the server key pair named by `server_key`, and the
   `key_id` inside matches the clear-text `key_id`.
3. `key_id` exists and is not revoked.
4. `sig` is valid for `message` with that key's public key.
5. `|now − ts| ≤ 300` seconds.
6. `nonce` was not seen for this key in the last 10 minutes.
7. The decoded payload validates against [report.schema.json](report.schema.json).

### Responses

| Status | Body | Meaning |
| --- | --- | --- |
| `202` | `{"status": "accepted"}` | queued; processing is asynchronous |
| `400` | `{"error": "bad_request"}` | checks 1, 2 or 7 failed |
| `401` | `{"error": "unauthorized"}` | checks 3 to 6 failed (deliberately not more specific) |
| `413` | `{"error": "too_large"}` | body over the limit |
| `429` | `{"error": "rate_limited"}` + `Retry-After` | more than 60 reports per key and hour |

A report identical to the previous one for the same environment only refreshes
its *last seen* date.

## On the client

- `stackscan link <code>` stores the signing key as a token
  `ssk1.<key_id>.<base64url Ed25519 seed>` in the user configuration folder
  (`~/Library/Application Support/stackscan/keys/` on macOS,
  `~/.config/stackscan/keys/` on Linux), mode `0600`, and records
  `{"environments": {"prod": "<key_id>"}}` in `.stackscan.json` at the project
  root. That file holds no secret and can be committed.
- In CI, the same token goes in a masked variable `STACKSCAN_KEY`.
- `STACKSCAN_URL` overrides the base URL (HTTPS required, except
  `http://localhost` for development). `STACKSCAN_SERVER_KEY="<id>:<base64>"`
  overrides the built-in server key, for a self-hosted or development server.

## Report

Described by [report.schema.json](report.schema.json). Example:

```json
{
  "schema": 1,
  "stackscan_version": "v0.4.0",
  "scanned_at": "2026-10-28T15:40:00Z",
  "git": { "commit": "99ec32e", "branch": "main" },
  "components": [
    { "eco": "runtime", "name": "postgresql", "version": "14", "source": "compose.yaml", "image": "postgres", "floating": true },
    { "eco": "image", "name": "postgres", "version": "14-alpine", "source": "compose.yaml", "floating": true },
    { "eco": "composer", "name": "symfony/framework-bundle", "version": "v7.4.16", "source": "app/composer.lock", "direct": true },
    { "eco": "composer", "name": "psr/log", "version": "3.0.2", "source": "app/composer.lock" },
    { "eco": "runtime", "name": "php", "version": ">=8.2", "source": "app/composer.json", "declared": true }
  ]
}
```

The report holds **no statuses**: no latest version, no end-of-life date.
stackscan cloud computes them, once per distinct version for all users, and
re-evaluates them every day, so a version reaching its end of life triggers an
alert without a new push.

The report holds no project name either: the server knows the project and
environment from `key_id`. Manifest paths (`source`) are stored encrypted.

## Versioning

- `v` versions the envelope, `schema` versions the report. Both are integers.
- New optional fields may be added to the report without changing `schema`;
  the server ignores fields it does not know.
- A breaking change gets a new number. The server accepts the current and
  previous numbers for at least six months.
