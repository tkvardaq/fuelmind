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
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
	inbound InboundHandler
}

// InboundHandler answers a message the station received. Returning an
// empty string sends nothing back, which is what an ignored message
// looks like.
//
// The handler is called on its own goroutine, one per message, so a slow
// answer never blocks the WhatsApp connection's event loop.
type InboundHandler func(ctx context.Context, from, text string) string

// inboundTimeout bounds how long one answer may take. A person is
// waiting on their phone; past this they would assume it is broken
// anyway, and WhatsApp itself will not hold the connection forever.
const inboundTimeout = 30 * time.Second

// maxInboundLength caps an incoming message before it is handed on.
const maxInboundLength = 1000

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
	// Listen before connecting, so a message that arrives during the
	// first handshake is not dropped.
	c.cli.AddEventHandler(c.handleEvent)

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

// SetInbound installs the handler that answers incoming messages. Until
// one is set, the station listens but says nothing — which is what an
// unconfigured station should do.
func (c *Client) SetInbound(h InboundHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inbound = h
}

// handleEvent is whatsmeow's callback. It runs on the connection's event
// loop, so it must return quickly: the actual answering happens on its
// own goroutine.
func (c *Client) handleEvent(evt any) {
	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}
	c.mu.Lock()
	handler := c.inbound
	c.mu.Unlock()
	if handler == nil {
		return
	}

	// Only one-to-one messages from another person. A station must never
	// answer into a group, reply to its own outgoing messages (which
	// would loop), or respond to a status broadcast.
	if msg.Info.IsFromMe || msg.Info.IsGroup || msg.Info.Chat.Server == types.BroadcastServer {
		return
	}
	text := messageText(msg)
	if text == "" {
		return // a photo, a sticker, a reaction: nothing to answer
	}
	if len(text) > maxInboundLength {
		text = text[:maxInboundLength]
	}
	from := msg.Info.Sender.User
	chat := msg.Info.Chat

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), inboundTimeout)
		defer cancel()
		reply := handler(ctx, from, text)
		if strings.TrimSpace(reply) == "" {
			return
		}
		if _, err := c.cli.SendMessage(ctx, chat, &waE2E.Message{
			Conversation: proto.String(reply),
		}); err != nil {
			c.log.Warn("whatsapp: could not send the reply", "to", from, "err", err)
		}
	}()
}

// messageText pulls the words out of the message shapes a person can
// actually type: a plain message, one with a link preview or a mention,
// and the caption on a photo.
func messageText(msg *events.Message) string {
	m := msg.Message
	if m == nil {
		return ""
	}
	switch {
	case m.GetConversation() != "":
		return strings.TrimSpace(m.GetConversation())
	case m.GetExtendedTextMessage().GetText() != "":
		return strings.TrimSpace(m.GetExtendedTextMessage().GetText())
	case m.GetImageMessage().GetCaption() != "":
		return strings.TrimSpace(m.GetImageMessage().GetCaption())
	case m.GetVideoMessage().GetCaption() != "":
		return strings.TrimSpace(m.GetVideoMessage().GetCaption())
	}
	return ""
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
