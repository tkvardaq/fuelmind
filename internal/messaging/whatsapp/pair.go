package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
)

// ErrAlreadyPaired means a phone is already linked; unlink it first.
var ErrAlreadyPaired = errors.New("whatsapp: this station is already paired with a phone")

// StartPairing begins a linking session and returns once the first QR
// code is available, so the page that called it has something to draw.
//
// The code rotates every ~20 seconds until the owner scans it, which is
// why the code is held here and read back by the dashboard rather than
// returned once.
func (c *Client) StartPairing(ctx context.Context) error {
	c.mu.Lock()
	if c.cli != nil && c.cli.Store.ID != nil {
		c.mu.Unlock()
		return ErrAlreadyPaired
	}
	cli := c.cli
	c.qrCode, c.pairErr = "", ""
	c.mu.Unlock()

	if cli == nil {
		return errors.New("whatsapp: client not started")
	}
	if cli.IsConnected() {
		cli.Disconnect()
	}

	// The QR channel must exist before Connect, or the server's pairing
	// challenge arrives with nowhere to go.
	qrChan, err := cli.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("whatsapp: start pairing: %w", err)
	}
	if err := cli.Connect(); err != nil {
		return fmt.Errorf("whatsapp: connect for pairing: %w", err)
	}

	first := make(chan struct{})
	go c.consumeQR(qrChan, first)

	select {
	case <-first:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("whatsapp: no pairing code arrived; check this machine's internet connection")
	}
}

// consumeQR keeps the displayed code current until pairing ends.
func (c *Client) consumeQR(qrChan <-chan whatsmeow.QRChannelItem, first chan struct{}) {
	announced := false
	announce := func() {
		if !announced {
			announced = true
			close(first)
		}
	}
	defer announce()

	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			c.mu.Lock()
			c.qrCode, c.pairErr = item.Code, ""
			c.qrUntil = time.Now().Add(item.Timeout)
			c.mu.Unlock()
			announce()
		case whatsmeow.QRChannelSuccess.Event:
			c.mu.Lock()
			c.qrCode, c.pairErr = "", ""
			c.mu.Unlock()
			c.log.Info("whatsapp paired", "number", c.Number())
			announce()
		default:
			msg := item.Event
			if item.Error != nil {
				msg = item.Error.Error()
			}
			c.mu.Lock()
			c.qrCode, c.pairErr = "", msg
			c.mu.Unlock()
			c.log.Warn("whatsapp pairing ended", "reason", msg)
			announce()
		}
	}
}

// PairingCode returns the QR payload the dashboard should draw, and the
// last pairing error if the attempt ended badly.
func (c *Client) PairingCode() (code, pairErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.qrCode != "" && time.Now().After(c.qrUntil.Add(5*time.Second)) {
		// Stale: the rotation stopped (window closed, network dropped).
		return "", c.pairErr
	}
	return c.qrCode, c.pairErr
}

// Unpair logs the station out of WhatsApp and forgets the session, so the
// owner can link a different phone.
func (c *Client) Unpair(ctx context.Context) error {
	c.mu.Lock()
	cli := c.cli
	c.qrCode, c.pairErr = "", ""
	c.mu.Unlock()
	if cli == nil || cli.Store.ID == nil {
		return ErrNotPaired
	}
	if err := cli.Logout(ctx); err != nil {
		// Logging out server-side can fail when offline; clearing the
		// local device still frees the station to pair again.
		c.log.Warn("whatsapp: logout", "err", err)
		if err := c.container.DeleteDevice(ctx, cli.Store); err != nil {
			return fmt.Errorf("whatsapp: forget pairing: %w", err)
		}
	}
	cli.Disconnect()

	device, err := c.container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp: reset device: %w", err)
	}
	fresh := whatsmeow.NewClient(device, cli.Log)
	fresh.EnableAutoReconnect = true
	c.mu.Lock()
	c.cli = fresh
	c.mu.Unlock()
	return nil
}
