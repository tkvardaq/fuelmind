// Package hashgen computes SHA-256 checksums of update artifacts.
// Used in Phase 6 (unattended updates) to generate the checksum that
// goes into the update manifest.
//
// Usage:
//
//	go run ./tools/hashgen <path-to-artifact.zip>
//
// Output: <sha256-hex>  <filename>
package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: hashgen <file>\n")
	}

	path := os.Args[1]
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("hashgen: open %q: %v", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Fatalf("hashgen: read %q: %v", path, err)
	}

	sum := h.Sum(nil)
	fmt.Printf("%x  %s\n", sum, filepath.Base(path))
}