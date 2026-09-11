package main

import (
	"context"
	"log"
	"os"

	"github.com/fuelmind/fuelmind/internal/launcher"
)

func main() {
	baseDir := os.Getenv("FUELMIND_BASE_DIR")
	if baseDir == "" {
		baseDir = `C:\ProgramData\FuelMind`
	}
	sup := launcher.NewSupervisor(baseDir)
	if err := sup.Run(context.Background()); err != nil {
		log.Fatalf("launcher exited: %v", err)
	}
}