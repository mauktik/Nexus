// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package release is the nexusd release-signing format.
//
// A release tarball is signed with Ed25519 (RFC 8032, pure, no prehash) over
// a domain-separated message: SignatureContext followed by the tarball's
// SHA-256 digest (A-1). The context keeps a release signature from being
// valid for any other use of the same key, and signing the digest rather
// than the tarball lets the daemon verify before it reads the archive. The
// format is reproducible with stock OpenSSL 3 (`pkeyutl -sign -rawin`), so a
// release can be signed or checked without Nexus tooling (CON-6).
//
// The package uses the standard library only.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// SignatureContext prefixes the digest in every signed message (A-1). The
// trailing NUL ends the context so no context is a prefix of another.
const SignatureContext = "nexus-release-signature-v1\x00"

const (
	// encodedPublicKeyLen is the standard base64 length of a 32-byte key.
	encodedPublicKeyLen = 44
	// encodedSignatureLen is the standard base64 length of a 64-byte
	// signature, without the .sig file's trailing newline.
	encodedSignatureLen = 88
	// maxKeyFileSize bounds the read of a private key file; a PKCS#8
	// Ed25519 PEM is about 120 bytes.
	maxKeyFileSize = 16 << 10
	// digestBufferSize is the DigestFile read buffer.
	digestBufferSize  = 32 << 10
	pemPrivateKeyType = "PRIVATE KEY"
)

// Message returns the bytes a release signature signs: SignatureContext
// followed by digest, 59 bytes.
func Message(digest [sha256.Size]byte) []byte {
	m := make([]byte, 0, len(SignatureContext)+len(digest))
	m = append(m, SignatureContext...)
	return append(m, digest[:]...)
}

// Sign signs Message(digest) with k. It panics, as ed25519.Sign does, if k is
// not ed25519.PrivateKeySize bytes long.
func Sign(k ed25519.PrivateKey, digest [sha256.Size]byte) []byte {
	return ed25519.Sign(k, Message(digest))
}

// Verify reports whether sig is a signature of Message(digest) by any key in
// keys. It never panics: a signature that is not 64 bytes, an empty key list
// and keys of the wrong length are all simply not a match.
func Verify(keys []ed25519.PublicKey, digest [sha256.Size]byte, sig []byte) bool {
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	m := Message(digest)
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(k, m, sig) {
			return true
		}
	}
	return false
}

// DigestFile returns the SHA-256 digest and size of the file at path,
// reading it through a 32 KiB buffer.
func DigestFile(path string) (digest [sha256.Size]byte, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return digest, 0, err
	}
	defer f.Close()
	h := sha256.New()
	// The anonymous struct hides *os.File's WriterTo so CopyBuffer uses buf.
	size, err = io.CopyBuffer(h, struct{ io.Reader }{f}, make([]byte, digestBufferSize))
	if err != nil {
		return digest, 0, fmt.Errorf("digest %s: %w", path, err)
	}
	h.Sum(digest[:0])
	return digest, size, nil
}

// ParsePublicKey parses a trustedKeys entry: the standard, padded base64
// encoding of a raw 32-byte Ed25519 public key, with no surrounding space.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	if len(b64) != encodedPublicKeyLen {
		return nil, fmt.Errorf("public key: want %d characters of standard base64, got %d", encodedPublicKeyLen, len(b64))
	}
	raw, err := decodeStrict(b64)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key: want %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// EncodePublicKey returns the trustedKeys form of k: standard base64 of the
// raw 32 bytes.
func EncodePublicKey(k ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k)
}

// EncodeSignature returns the .sig file content for sig: standard base64 and
// a newline.
func EncodeSignature(sig []byte) []byte {
	out := make([]byte, base64.StdEncoding.EncodedLen(len(sig))+1)
	base64.StdEncoding.Encode(out, sig)
	out[len(out)-1] = '\n'
	return out
}

// DecodeSignature parses .sig file content: 88 characters of standard
// base64 encoding 64 bytes, optionally followed by one newline.
func DecodeSignature(b []byte) ([]byte, error) {
	s := strings.TrimSuffix(string(b), "\n")
	if len(s) != encodedSignatureLen {
		return nil, fmt.Errorf("signature: want %d characters of standard base64, got %d", encodedSignatureLen, len(s))
	}
	sig, err := decodeStrict(s)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature: want %d bytes, got %d", ed25519.SignatureSize, len(sig))
	}
	return sig, nil
}

// decodeStrict decodes padded standard base64, refusing the CR and LF that
// encoding/base64 otherwise skips and any non-zero padding bits.
func decodeStrict(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("not standard base64: contains a line break")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		// The CorruptInputError names only an offset, never input bytes.
		return nil, fmt.Errorf("not standard base64: %w", err)
	}
	return raw, nil
}

// LoadPrivateKey reads a PEM PKCS#8 Ed25519 private key. It refuses a file
// whose mode allows anything beyond owner read and write (so 0600 and 0400
// load; any group or other bit, or an execute bit, is refused) and any key
// type other than Ed25519. Errors name the path and mode, never key
// material.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Stat the open file, not the path, so the mode checked is the mode of
	// the bytes read.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&^0o600 != 0 {
		return nil, fmt.Errorf("%s: mode %04o is too open; a signing key must be readable by its owner only (chmod 600)", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxKeyFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes; not a private key file", path, maxKeyFileSize)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != pemPrivateKeyType {
		return nil, fmt.Errorf("%s: no PEM %s block", path, pemPrivateKeyType)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse PKCS#8: %w", path, err)
	}
	k, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key (%T)", path, key)
	}
	return k, nil
}

// WritePrivateKey writes k to path as a PEM PKCS#8 block at mode 0600. It
// refuses an existing path (the error wraps fs.ErrExist) and never replaces
// a key. On a write failure the partial file is removed.
func WritePrivateKey(path string, k ed25519.PrivateKey) (err error) {
	if len(k) != ed25519.PrivateKeySize {
		return fmt.Errorf("write %s: want a %d-byte ed25519 private key, got %d bytes", path, ed25519.PrivateKeySize, len(k))
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("write %s: %w", path, cerr)
		}
		if err != nil {
			if rerr := os.Remove(path); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	// The umask can only clear bits; Chmod makes the mode exactly 0600.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: pemPrivateKeyType, Bytes: der}); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
