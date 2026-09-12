// Fake FuelMindCore.exe for launcher tests. Behaviour per version dir via
// CHILD_MODE_<version> = "exit:N" | "sleep" | "serve" (answers /healthz on
// FUELMIND_PORT). "-version" prints CHILD_BOOT_VERSION.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Printf("fuelmind-core %s\n", os.Getenv("CHILD_BOOT_VERSION"))
		return
	}
	exe, _ := os.Executable()
	ver := filepath.Base(filepath.Dir(exe))
	if p := os.Getenv("CHILD_LOG"); p != "" {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		_, _ = f.WriteString(ver + "\n")
		_ = f.Close()
	}
	mode := os.Getenv("CHILD_MODE_" + strings.NewReplacer(".", "_", "-", "_").Replace(ver))
	switch {
	case strings.HasPrefix(mode, "exit:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(mode, "exit:"))
		os.Exit(n)
	case mode == "serve":
		http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
		go func() { _ = http.ListenAndServe("127.0.0.1:"+os.Getenv("FUELMIND_PORT"), nil) }()
	}
	// Exit when stdin closes (the launcher's graceful stop), like the core.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Hour):
	}
}
