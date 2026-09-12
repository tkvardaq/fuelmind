// Command fuelmind-launcher is the FuelMind Windows service. It installs
// the bundled core on first run, supervises it, and applies unattended
// updates (see internal/launcher).
//
// Run from a console it behaves the same way until Ctrl+C.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/fuelmind/fuelmind/internal/launcher"
)

// ServiceName must match ServiceInstall/@Name in installer/product.wxs.
const ServiceName = "FuelMindService"

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("fuelmind-launcher %s\n", version)
		return
	}

	baseDir := dataDir()
	isService, err := launcher.IsWindowsService()
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: cannot determine session type: %v\n", err)
		os.Exit(1)
	}

	// Under the SCM there is no console: writing to stderr fails, and an
	// io.MultiWriter would stop at that error and never reach the file.
	// So a service logs to the file only.
	logFile, logErr := launcher.OpenRotatingLog(filepath.Join(baseDir, "logs"), "launcher.log")
	var out io.Writer = os.Stderr
	if logErr == nil {
		defer logFile.Close()
		if isService {
			out = logFile
		} else {
			out = io.MultiWriter(os.Stderr, logFile)
		}
	}
	logger := log.New(out, "launcher: ", log.LstdFlags)
	if logErr != nil {
		logger.Printf("cannot open launcher.log: %v", logErr)
	}

	exe, _ := os.Executable()
	port := 8765
	if p, err := strconv.Atoi(os.Getenv("FUELMIND_PORT")); err == nil && p > 0 {
		port = p
	}
	sup := launcher.New(launcher.Options{
		BaseDir:    baseDir,
		InstallDir: filepath.Dir(exe),
		Port:       port,
		Logger:     logger,
	})
	logger.Printf("starting %s (data dir %s)", version, baseDir)

	if isService {
		if err := launcher.RunService(ServiceName, sup.Run); err != nil {
			logger.Fatalf("service: %v", err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sup.Run(ctx); err != nil {
		logger.Fatalf("%v", err)
	}
}

// dataDir resolves the same data directory the core uses.
func dataDir() string {
	for _, k := range []string{"FUELMIND_BASE_DIR", "FUELMIND_DATA_DIR"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "FuelMind")
	}
	return `C:\ProgramData\FuelMind`
}
