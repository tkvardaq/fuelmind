// Command fuelmind-setup is the station support tool. Run it on the shop
// PC as administrator.
//
//	fuelmind-setup status          show station id, tier, data dir, POS folder
//	fuelmind-setup reset-pin       set a new dashboard PIN (prompts twice)
//	fuelmind-setup test-heartbeat  send one heartbeat to the configured cloud
//
// The dashboard's own /setup page handles the first PIN; this tool exists
// for "the owner forgot the PIN" and for on-site diagnosis.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/auth"
	"github.com/fuelmind/fuelmind/internal/config"
	"github.com/fuelmind/fuelmind/internal/hardware"
	"github.com/fuelmind/fuelmind/internal/ident"
	"github.com/fuelmind/fuelmind/internal/relay"
	"github.com/fuelmind/fuelmind/internal/storage"
	"github.com/fuelmind/fuelmind/internal/sync"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fuelmind-setup:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: fuelmind-setup status | reset-pin | test-heartbeat | remote-ask on|off")
}

func run(args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usage()
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", cfg.Path, err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	switch args[0] {
	case "status":
		return status(ctx, store, cfg)
	case "reset-pin":
		return resetPIN(ctx, store)
	case "test-heartbeat":
		return testHeartbeat(ctx, store, cfg)
	case "remote-ask":
		return remoteAsk(ctx, store, args)
	default:
		return usage()
	}
}

func status(ctx context.Context, store *storage.Storage, cfg *config.Config) error {
	tier := store.LocalConfigValue(ctx, ident.ConfigKeyHardwareTier, hardware.DetectHardwareTier())
	fmt.Println("FuelMind station status")
	fmt.Println("  station id:   ", store.LocalConfigValue(ctx, ident.ConfigKeyStationID, "(not generated yet)"))
	fmt.Println("  licence tier: ", sync.LicenseTier(ctx, store))
	fmt.Println("  hardware tier:", tier)
	fmt.Println("  data dir:     ", cfg.DataDir)
	fmt.Println("  dashboard:     http://localhost:" + fmt.Sprint(cfg.Port) + "/")
	if cfg.SyncEnabled {
		fmt.Println("  cloud:        ", cfg.CloudURL)
	} else {
		fmt.Println("  cloud:         not configured (local-only)")
	}
	fmt.Println("  telemetry:    ", store.LocalConfigValue(ctx, sync.ConfigKeyTelemetryConsent, "false"))
	remote := "off"
	if relay.Enabled(ctx, store) {
		remote = "on"
	}
	fmt.Println("  remote questions:", remote, "(fuelmind-setup remote-ask on|off)")

	// Every folder the station watches, not just the built-in one: the
	// owner points FuelMind at wherever their POS already writes, so
	// reporting only the built-in folder would say "0 processed" while
	// the real data is flowing in somewhere else.
	folders, ferr := store.WatchFolders(ctx)
	switch {
	case ferr != nil:
		fmt.Println("  POS folders:   could not be read:", ferr)
	case len(folders) == 0:
		fmt.Println("  POS folders:   none configured (add one on the dashboard's Data page)")
	default:
		fmt.Println("  POS folders:")
		for _, f := range folders {
			state := "watching"
			if !f.Enabled {
				state = "switched off"
			}
			pending, err := countCSVs(f.Path)
			if err != nil {
				fmt.Printf("    %-6s %s  NOT USABLE: %v\n", state, f.Path, err)
				continue
			}
			processed, _ := countCSVs(filepath.Join(f.Path, "processed"))
			failed, _ := countCSVs(filepath.Join(f.Path, "failed"))
			fmt.Printf("    %s  %s (%d waiting, %d processed, %d failed)\n", state, f.Path, pending, processed, failed)
			if f.LastError != "" {
				fmt.Printf("           last problem: %s\n", f.LastError)
			}
		}
	}
	if last, err := store.LastPosIngestionAt(ctx); err == nil && !last.IsZero() {
		fmt.Println("  last POS file:", last.Local().Format(time.RFC1123))
	} else {
		fmt.Println("  last POS file: none ingested yet")
	}
	if unresolved, err := store.UnresolvedProducts(ctx); err == nil && len(unresolved) > 0 {
		fmt.Println("  unknown product names (rows are kept, add an alias to import them):")
		for _, u := range unresolved {
			fmt.Printf("    %-24s %d row(s)\n", u.Alias, u.Rows)
		}
	}
	if a, err := store.LatestSyncAttempt(ctx); err == nil {
		fmt.Printf("  last heartbeat: %s (%s %s)\n", a.SentAt.Local().Format(time.RFC1123), a.Status, a.Reason)
	}
	return nil
}

func countCSVs(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			n++
		}
	}
	return n, nil
}

func resetPIN(ctx context.Context, store *storage.Storage) error {
	pin, err := readPIN("New dashboard PIN (8 or more characters): ")
	if err != nil {
		return err
	}
	again, err := readPIN("Repeat the PIN: ")
	if err != nil {
		return err
	}
	if pin != again {
		return errors.New("the two PINs do not match; nothing was changed")
	}
	if err := auth.New(store).SetupPIN(ctx, pin); err != nil {
		return err
	}
	if err := store.DeleteAllSessions(ctx); err != nil {
		return fmt.Errorf("PIN changed, but signing other devices out failed: %w", err)
	}
	fmt.Println("PIN updated. Everyone signed in on other devices has been signed out.")
	return nil
}

// readPIN reads one line without echoing it, and fails on EOF instead of
// looping (so a silent/unattended run cannot spin forever).
func readPIN(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := readLineNoEcho(os.Stdin)
	fmt.Println()
	if err != nil {
		return "", err
	}
	pin := strings.TrimSpace(line)
	if len(pin) < auth.MinPINLength {
		return "", auth.ErrPINTooShort
	}
	return pin, nil
}

func readLine(f *os.File) (string, error) {
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no input (run this tool in a console)")
	}
	return line, nil
}

func testHeartbeat(ctx context.Context, store *storage.Storage, cfg *config.Config) error {
	if !cfg.SyncEnabled {
		return errors.New("no cloud configured (FUELMIND_CLOUD_URL is unset), so there is nothing to contact")
	}
	sid := store.LocalConfigValue(ctx, ident.ConfigKeyStationID, "")
	key := store.LocalConfigValue(ctx, ident.ConfigKeyAPIKey, "")
	if sid == "" || key == "" {
		return errors.New("this station has no identity yet; start the FuelMind service once first")
	}
	agent := sync.NewAgent(sync.AgentConfig{
		Store:           store,
		Client:          sync.NewHTTPClient(cfg.CloudURL),
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Identity:        ident.Identity{StationID: ident.StationID(sid), APIKey: ident.APIKey(key)},
		SoftwareVersion: "setup-tool",
	})
	fmt.Printf("Sending a heartbeat to %s ...\n", cfg.CloudURL)
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := agent.SendOnce(sendCtx); err != nil {
		return fmt.Errorf("heartbeat failed: %w", err)
	}
	fmt.Println("Heartbeat accepted. Licence tier:", sync.LicenseTier(ctx, store))
	return nil
}

// remoteAsk turns answering questions from the owner's phone on or off.
func remoteAsk(ctx context.Context, store *storage.Storage, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: fuelmind-setup remote-ask on|off")
	}
	switch args[1] {
	case "on":
		if err := relay.SetEnabled(ctx, store, true); err != nil {
			return err
		}
		fmt.Println("Remote questions are ON.")
		fmt.Println("The station will answer questions sent by WhatsApp, SMS or support.")
		fmt.Println("Answers contain figures such as today's revenue and pass through the FuelMind service.")
	case "off":
		if err := relay.SetEnabled(ctx, store, false); err != nil {
			return err
		}
		fmt.Println("Remote questions are OFF. Questions can only be asked on the station network.")
	default:
		return errors.New("usage: fuelmind-setup remote-ask on|off")
	}
	return nil
}
