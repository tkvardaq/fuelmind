// Package whatsapp sends FuelMind's messages over WhatsApp using
// whatsmeow (https://github.com/tulir/whatsmeow), the open-source Go
// implementation of the WhatsApp Web protocol.
//
// Why this and not the official Business API: a pilot station is one
// owner with one phone. The official route needs a Meta business
// verification, a BSP account and per-message billing before a single
// message can be sent. whatsmeow needs the owner to scan a QR code once,
// costs nothing, and keeps the station in control of its own number.
//
// The trade-off is honest and worth stating: this is an unofficial
// client. WhatsApp does not support it, and an account that sends
// unsolicited volume can be banned. FuelMind only ever messages the one
// number the owner entered, and only about their own station, which is
// well inside normal personal use.
//
// The pairing session lives in its own SQLite file next to the FuelMind
// database, so re-pairing never touches station data.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite" // pure-Go driver, same as the main store
)

// Client is a paired WhatsApp connection. The zero value is not usable;
// call Open.
type Client struct {
	log       *slog.Logger
	db        *sql.DB
	container *sqlstore.Container

	mu      sync.Mutex
	cli     *whatsmeow.Client
	qrCode  string    // the code currently displayed for pairing, if any
	qrUntil time.Time // when that code expires
	pairErr string
}

// SessionFile is the pairing database, kept beside the main one.
const SessionFile = "whatsapp.db"

// Open loads the saved pairing from dir, connecting if one exists. A
// station that has never been paired opens fine and reports NotPaired;
// the owner pairs it from the dashboard.
func Open(ctx context.Context, dir string, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	path := filepath.Join(dir, SessionFile)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open session db: %w", err)
	}
	// One connection: whatsmeow writes its session state constantly and
	// SQLite is happier serialized than contended.
	db.SetMaxOpenConns(1)

	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: prepare session db: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: read device: %w", err)
	}

	c := &Client{log: logger, db: db, container: container}
	c.cli = whatsmeow.NewClient(device, waLog.Noop)
	c.cli.EnableAutoReconnect = true

	if c.cli.Store.ID != nil {
		if err := c.cli.Connect(); err != nil {
			// Not fatal: the station runs, messages stay queued, and the
			// next tick or a reconnect picks it up.
			logger.Warn("whatsapp: could not connect with the saved pairing", "err", err)
		}
	}
	return c, nil
}

// Name identifies the channel in the outbox.
func (c *Client) Name() string { return "whatsapp" }

// Paired reports whether a phone has been linked to this station.
func (c *Client) Paired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli != nil && c.cli.Store != nil && c.cli.Store.ID != nil
}

// Ready reports whether a message can go out right now.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli != nil && c.cli.IsLoggedIn() && c.cli.IsConnected()
}

// Number returns the paired phone number, or "".
func (c *Client) Number() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == nil || c.cli.Store == nil || c.cli.Store.ID == nil {
		return ""
	}
	return c.cli.Store.ID.User
}

// ErrNotPaired means no phone has been linked yet.
var ErrNotPaired = errors.New("whatsapp: this station is not paired with a phone yet")

// Send delivers body to a phone number in international format, digits
// only (e.g. "923001234567").
func (c *Client) Send(ctx context.Context, to, body string) error {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil || cli.Store.ID == nil {
		return ErrNotPaired
	}
	if !cli.IsConnected() {
		if err := cli.Connect(); err != nil {
			return fmt.Errorf("whatsapp: connect: %w", err)
		}
	}
	jid := types.NewJID(to, types.DefaultUserServer)
	if _, err := cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(body)}); err != nil {
		return fmt.Errorf("whatsapp: send to %s: %w", to, err)
	}
	return nil
}

// Close disconnects and releases the session database.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil {
		c.cli.Disconnect()
	}
	if c.container != nil {
		return c.container.Close()
	}
	return c.db.Close()
}
