// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Command nexus-release manages the nexusd release-signing key and signs
// and checks release tarballs in the internal/release format (FR-15).
//
//	nexus-release keygen    [--key P]   make a key; print the trustedKeys line
//	nexus-release pubkey    [--key P]   print the key's public half
//	nexus-release check-key [--key P]   exit 1 naming why the key is unusable
//	nexus-release sign      [--key P] FILE...   write FILE.sig for each FILE
//	nexus-release verify    --pubkey B64 FILE   check FILE.sig against B64
//
// The key path defaults to $NEXUS_RELEASE_KEY, else
// ~/.config/nexus/release-signing.key. `make release-key` runs keygen and
// `make release` runs check-key before anything else, then sign. The tool
// never prints private key material.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mauktik/nexus/internal/release"
)

const name = "nexus-release"

// keyEnv names the key path when --key is not given.
const keyEnv = "NEXUS_RELEASE_KEY"

// defaultKey is the key path under $HOME when neither --key nor keyEnv is set.
var defaultKey = filepath.Join(".config", "nexus", "release-signing.key")

// maxSigFileSize bounds the read of a .sig file: 88 base64 characters and a
// newline.
const maxSigFileSize = 1 << 10

const usage = `usage:
  nexus-release keygen    [--key P]
  nexus-release pubkey    [--key P]
  nexus-release check-key [--key P]
  nexus-release sign      [--key P] FILE...
  nexus-release verify    --pubkey B64 FILE

The key path defaults to $NEXUS_RELEASE_KEY, else ~/.config/nexus/release-signing.key.
`

// errUsage is reported as exit status 2.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run executes one subcommand with getenv as its environment. It returns
// the exit status: 0 on success, 1 on a failure and 2 on a usage error.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmds := map[string]func([]string, func(string) string, io.Writer, io.Writer) error{
		"keygen":    keygen,
		"pubkey":    pubkey,
		"check-key": checkKey,
		"sign":      sign,
		"verify":    verify,
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "%s: unknown command %q\n%s", name, args[0], usage)
		return 2
	}
	err := cmd(args[1:], getenv, stdout, stderr)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(stderr, "%s %s: %v\n", name, args[0], err)
		return 1
	}
}

// flags parses a subcommand's flags; a parse error is a usage error.
func flags(fset *flag.FlagSet, args []string, stderr io.Writer) error {
	fset.SetOutput(stderr)
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	return nil
}

// keyFlag adds --key to fs.
func keyFlag(fset *flag.FlagSet) *string {
	return fset.String("key", "", "the private key `path` (default $"+keyEnv+", else ~/"+filepath.ToSlash(defaultKey)+")")
}

// keyPath resolves the key path: --key, then $NEXUS_RELEASE_KEY, then the
// default under $HOME.
func keyPath(flagValue string, getenv func(string) string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if p := getenv(keyEnv); p != "" {
		return p, nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("HOME is not set; pass --key or set $%s", keyEnv)
	}
	return filepath.Join(home, defaultKey), nil
}

// noArgs refuses positional arguments after a subcommand's flags.
func noArgs(fset *flag.FlagSet, stderr io.Writer) error {
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "%s %s: unexpected argument %q\n", name, fset.Name(), fset.Arg(0))
		return errUsage
	}
	return nil
}

// keygen makes a new key at the key path and prints the trustedKeys line for
// its public half. It never replaces an existing key.
func keygen(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("keygen", flag.ContinueOnError)
	keyArg := keyFlag(fset)
	if err := flags(fset, args, stderr); err != nil {
		return err
	}
	if err := noArgs(fset, stderr); err != nil {
		return err
	}
	path, err := keyPath(*keyArg, getenv)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists; refusing to replace a signing key", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := keyDir(filepath.Dir(path)); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate a key: %w", err)
	}
	// WritePrivateKey opens with O_EXCL, so a key that appeared since the
	// check above is still not replaced.
	if err := release.WritePrivateKey(path, priv); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to replace a signing key", path)
		}
		return err
	}
	fmt.Fprintf(stderr, "%s: wrote %s (mode 0600). Add this line to providers.nexusd.update in nexusd.yaml on each host:\n", name, path)
	_, err = fmt.Fprintf(stdout, "trustedKeys: [%q]\n", release.EncodePublicKey(pub))
	return err
}

// keyDir creates dir, and any missing parents, at mode 0700. An existing
// directory is left as it is.
func keyDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// MkdirAll's mode is filtered by the umask; make the key directory
	// exactly 0700.
	return os.Chmod(dir, 0o700)
}

// loadKey loads the private key at the resolved key path, naming a missing
// key as such.
func loadKey(flagValue string, getenv func(string) string) (ed25519.PrivateKey, string, error) {
	path, err := keyPath(flagValue, getenv)
	if err != nil {
		return nil, "", err
	}
	k, err := release.LoadPrivateKey(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, path, fmt.Errorf("%s: missing: no signing key there; create one with `make release-key` or `%s keygen --key %s`", path, name, path)
	}
	return k, path, err
}

// pubkey prints the key's public half in trustedKeys form.
func pubkey(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	keyArg := keyFlag(fset)
	if err := flags(fset, args, stderr); err != nil {
		return err
	}
	if err := noArgs(fset, stderr); err != nil {
		return err
	}
	k, _, err := loadKey(*keyArg, getenv)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, release.EncodePublicKey(k.Public().(ed25519.PublicKey)))
	return err
}

// checkKey succeeds only when the key loads: present, a regular file of
// mode 0600 or stricter, and a PKCS#8 Ed25519 key.
func checkKey(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("check-key", flag.ContinueOnError)
	keyArg := keyFlag(fset)
	if err := flags(fset, args, stderr); err != nil {
		return err
	}
	if err := noArgs(fset, stderr); err != nil {
		return err
	}
	k, path, err := loadKey(*keyArg, getenv)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s: %s: an ed25519 signing key; public key %s\n", name, path, release.EncodePublicKey(k.Public().(ed25519.PublicKey)))
	return err
}

// sign writes FILE.sig for each FILE, checking each signature against the
// key's public half before it is written.
func sign(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyArg := keyFlag(fset)
	if err := flags(fset, args, stderr); err != nil {
		return err
	}
	if fset.NArg() == 0 {
		fmt.Fprintf(stderr, "%s sign: name at least one FILE\n", name)
		return errUsage
	}
	k, _, err := loadKey(*keyArg, getenv)
	if err != nil {
		return err
	}
	pub := k.Public().(ed25519.PublicKey)
	for _, file := range fset.Args() {
		digest, _, err := release.DigestFile(file)
		if err != nil {
			return err
		}
		sig := release.Sign(k, digest)
		if !release.Verify([]ed25519.PublicKey{pub}, digest, sig) {
			return fmt.Errorf("%s: the new signature does not verify; nothing written", file)
		}
		if err := os.WriteFile(file+".sig", release.EncodeSignature(sig), 0o644); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "%s: signed %s -> %s.sig\n", name, file, file); err != nil {
			return err
		}
	}
	return nil
}

// verify checks FILE.sig against one public key.
func verify(args []string, _ func(string) string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("verify", flag.ContinueOnError)
	pubArg := fset.String("pubkey", "", "the public key, in trustedKeys form (standard base64 of 32 bytes)")
	if err := flags(fset, args, stderr); err != nil {
		return err
	}
	if *pubArg == "" || fset.NArg() != 1 {
		fmt.Fprintf(stderr, "%s verify: want --pubkey B64 and exactly one FILE\n", name)
		return errUsage
	}
	pub, err := release.ParsePublicKey(*pubArg)
	if err != nil {
		return err
	}
	file := fset.Arg(0)
	sig, err := readSig(file + ".sig")
	if err != nil {
		return err
	}
	digest, _, err := release.DigestFile(file)
	if err != nil {
		return err
	}
	if !release.Verify([]ed25519.PublicKey{pub}, digest, sig) {
		return fmt.Errorf("%s: the signature in %s.sig does not verify against %s", file, file, *pubArg)
	}
	_, err = fmt.Fprintf(stdout, "%s: %s: signature verified\n", name, file)
	return err
}

// readSig reads and decodes a .sig file, bounding the read.
func readSig(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSigFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxSigFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes; not a signature file", path, maxSigFileSize)
	}
	sig, err := release.DecodeSignature(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sig, nil
}
