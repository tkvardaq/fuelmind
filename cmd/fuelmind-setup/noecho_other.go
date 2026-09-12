//go:build !windows

package main

import "os"

// readLineNoEcho falls back to a plain read outside Windows; the tool is
// a Windows support utility.
func readLineNoEcho(f *os.File) (string, error) { return readLine(f) }
