package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/config"
	"github.com/fuelmind/fuelmind/internal/hardware"
	"github.com/fuelmind/fuelmind/internal/storage"
)

func main() {
	fmt.Println("=== FuelMind First-Run Wizard ===")

	// Load config to get the data directory and DB path.
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Open the storage (SQLite) using the path from config.
	store, err := storage.Open(cfg.Path)
	if err != nil {
		fmt.Printf("Failed to open storage: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	// Run any pending migrations (creates dashboard_users, etc.)
	if err := store.Migrate(context.Background()); err != nil {
		fmt.Printf("Failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	// 1. PIN setup
	reader := bufio.NewReader(os.Stdin)
	authService := auth.New(store)
	for {
		fmt.Print("Enter a new PIN (minimum 4 digits): ")
		pinInput, _ := reader.ReadString('\n')
		pinInput = strings.TrimSpace(pinInput)
		if len(pinInput) < 4 {
			fmt.Println("PIN must be at least 4 digits. Try again.")
			continue
		}
		// Store PIN using auth package
		if err := authService.SetupPIN(context.Background(), pinInput); err != nil {
			fmt.Printf("Failed to set PIN: %v\n", err)
			continue
		}
		fmt.Println("PIN set successfully.")
		break
	}

	// 2. Hardware tier detection
	tier := hardware.DetectHardwareTier()
	fmt.Printf("Detected hardware tier: %s\n", tier)

	// Persist hardware tier in local config (if not already)
	existing, err := store.GetLocalConfig(context.Background(), "hardware_tier")
	if err != nil && err != storage.ErrNotFound {
		fmt.Printf("Warning: failed to check existing hardware tier: %v\n", err)
	} else if err == nil {
		// Already set, skip
		fmt.Printf("Hardware tier already set to %s\n", existing.Value)
	} else {
		// Not set, store it
		if err := store.SetLocalConfig(context.Background(), "hardware_tier", string(tier), "", false); err != nil {
			fmt.Printf("Warning: failed to persist hardware tier: %v\n", err)
		} else {
			fmt.Println("Hardware tier saved.")
		}
	}

	// 3. POS connection test (placeholder)
	fmt.Print("Testing POS connection (placeholder)... ")
	// In a real implementation, we would attempt to read from the POS adapter.
	fmt.Println("OK (placeholder)")

	// 4. Test heartbeat button (prompt)
	fmt.Print("Would you like to send a test heartbeat now? (y/N): ")
	resp, _ := reader.ReadString('\n')
	resp = strings.TrimSpace(strings.ToLower(resp))
	if resp == "y" || resp == "yes" {
		fmt.Println("Sending test heartbeat...")
		// Use sync agent to send a heartbeat (maybe a dry-run)
		// For simplicity, we just call the heartbeat endpoint if configured.
		// This is a placeholder; actual implementation would use sync.SendHeartbeat.
		fmt.Println("Test heartbeat sent (placeholder).")
	}

	fmt.Println("\nSetup complete. The FuelMind service will start automatically.")
}