//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// readLineNoEcho reads one line with console echo turned off, so a PIN
// typed on the shop PC is not left on screen.
func readLineNoEcho(f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return readLine(f) // not a console (piped input)
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return readLine(f)
	}
	defer windows.SetConsoleMode(h, mode)
	return readLine(f)
}
