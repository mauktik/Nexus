// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Command staticcheck fails unless every ELF file it is given is statically
// linked (FR-38): no PT_INTERP program header and no DT_NEEDED entry. A
// release binary with either needs a dynamic loader and libc at run time, so
// it would not start on a musl host such as Alpine. `make build-release` runs
// it on the linux release binaries.
//
//	staticcheck FILE...
//
// It names each failing file on stderr and exits 1 if any file fails, and 2
// when no file is given.
package main

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const name = "staticcheck"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(files []string, stderr io.Writer) int {
	if len(files) == 0 {
		fmt.Fprintf(stderr, "usage: %s FILE...\n", name)
		return 2
	}
	code := 0
	for _, f := range files {
		if err := check(f); err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", name, f, err)
			code = 1
		}
	}
	return code
}

// check returns an error naming every dynamic-linking marker in the ELF file
// at path, or the reason it could not be read.
func check(path string) (err error) {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	var found []string
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			interp, rerr := io.ReadAll(p.Open())
			if rerr != nil {
				return fmt.Errorf("read PT_INTERP: %w", rerr)
			}
			found = append(found, fmt.Sprintf("PT_INTERP %s", strings.TrimRight(string(interp), "\x00")))
		}
	}
	needed, err := f.DynString(elf.DT_NEEDED)
	if err != nil {
		return fmt.Errorf("read DT_NEEDED: %w", err)
	}
	if len(needed) > 0 {
		found = append(found, fmt.Sprintf("DT_NEEDED %s", strings.Join(needed, ", ")))
	}
	if len(found) > 0 {
		return fmt.Errorf("is not statically linked: %s", strings.Join(found, "; "))
	}
	return nil
}
