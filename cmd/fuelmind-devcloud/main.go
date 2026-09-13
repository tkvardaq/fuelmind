// Command fuelmind-devcloud runs the in-memory control plane
// (internal/cloudctl) as a real HTTP server, for development and for
// rehearsing an update before it goes to a station.
//
//	fuelmind-devcloud -db C:\ProgramData\FuelMind\fuelmind.db \
//	                  -release 1.1.0=dist\fuelmind-core-1.1.0.zip \
//	                  -admin-token secret
//
// -db registers the station whose identity is in that database, so the
// core can authenticate without any manual copying of keys. Artifacts are
// served from /artifacts/ so a release URL can be a plain relative path.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fuelmind/fuelmind/internal/cloudctl"
	_ "modernc.org/sqlite"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var stations, releases stringList
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dbPath := flag.String("db", "", "register the station from this fuelmind.db")
	adminToken := flag.String("admin-token", "", "bearer token for /v1/admin (empty disables the admin API)")
	webhookToken := flag.String("webhook-token", "", "shared secret for the WhatsApp/SMS webhook (empty disables it)")
	artifacts := flag.String("artifacts", "", "directory served at /artifacts/ (default: the folder of the first -release)")
	tier := flag.String("tier", "private", "licence tier for registered stations")
	flag.Var(&stations, "station", "station to register as ID=APIKEY (repeatable)")
	flag.Var(&releases, "release", "release to publish as VERSION=path-to-artifact.zip (repeatable)")
	flag.Parse()

	srv := cloudctl.New()
	srv.AdminToken = *adminToken
	srv.WebhookToken = *webhookToken

	if *dbPath != "" {
		id, key, err := identityFromDB(*dbPath)
		if err != nil {
			log.Fatalf("devcloud: %v", err)
		}
		stations = append(stations, id+"="+key)
	}
	if len(stations) == 0 {
		log.Fatal("devcloud: no stations; pass -db or -station ID=APIKEY")
	}
	for _, s := range stations {
		id, key, ok := strings.Cut(s, "=")
		if !ok {
			log.Fatalf("devcloud: -station %q must be ID=APIKEY", s)
		}
		srv.AddStation(&cloudctl.Station{
			StationID: id, APIKey: key, Tier: *tier, Status: "active",
			FeaturesJSON: map[string]any{"whatsapp_enabled": false, "cloud_backup_enabled": false},
			ValidFrom:    time.Now(),
		})
		log.Printf("station %s registered (tier %s)", id, *tier)
	}

	for _, r := range releases {
		v, path, ok := strings.Cut(r, "=")
		if !ok {
			log.Fatalf("devcloud: -release %q must be VERSION=path", r)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			log.Fatalf("devcloud: %v", err)
		}
		if *artifacts == "" {
			*artifacts = filepath.Dir(path)
		}
		srv.AddRelease(cloudctl.UpdateRelease{
			Version:        v,
			ArtifactURL:    "/artifacts/" + filepath.Base(path),
			ChecksumSHA256: sum,
			RolloutPct:     100,
		})
		log.Printf("release %s published (%s, sha256 %s...)", v, filepath.Base(path), sum[:12])
	}

	mux := http.NewServeMux()
	mux.Handle("/", logRequests(srv.Routes()))
	if *artifacts != "" {
		mux.Handle("/artifacts/", logRequests(http.StripPrefix("/artifacts/", http.FileServer(http.Dir(*artifacts)))))
		log.Printf("serving artifacts from %s", *artifacts)
	}
	if *webhookToken != "" {
		log.Printf("WhatsApp/SMS webhook: POST http://%s/v1/whatsapp/webhook?station_id=<ID>&token=<webhook token>", *addr)
	}
	log.Printf("devcloud listening on http://%s", *addr)
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("devcloud: %v", err)
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d", r.Method, r.URL.RequestURI(), rec.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }

func identityFromDB(path string) (id, key string, err error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	q := `SELECT value FROM local_config WHERE key = ?`
	if err := db.QueryRow(q, "station_id").Scan(&id); err != nil {
		return "", "", fmt.Errorf("read station_id from %s: %w", path, err)
	}
	if err := db.QueryRow(q, "api_key").Scan(&key); err != nil {
		return "", "", fmt.Errorf("read api_key from %s: %w", path, err)
	}
	return id, key, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
