// Package litestream is a client for the IPC socket of the Litestream process
// that replicates Cermin's database: only the call Cermin uses.
package litestream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// timeout bounds every call.
const timeout = 10 * time.Second

// Client calls Litestream about one database.
type Client struct {
	http   *http.Client
	dbPath string
}

// New returns a client for the database at dbPath, through Litestream's IPC
// socket at socketPath.
func New(socketPath, dbPath string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		http:   &http.Client{Transport: &http.Transport{DialContext: dial}},
		dbPath: dbPath,
	}
}

// Position is how far the database's changes have got.
type Position struct {
	// TXID is the last transaction Litestream has copied out of the WAL.
	TXID uint64 `json:"txid"`
	// ReplicatedTXID is the last transaction on the replica, or 0 while
	// Litestream doesn't know it, as after a failed upload.
	ReplicatedTXID uint64 `json:"replicated_txid"`
}

// Sync has Litestream copy the database's new changes out of the WAL at once,
// as it does every second anyway, and returns the Position after, without
// waiting for the replica.
func (c *Client) Sync(ctx context.Context) (Position, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"path": c.dbPath})
	if err != nil {
		return Position{}, fmt.Errorf("litestream: sync: encoding request: %w", err)
	}
	// The host is ignored: every request goes to the socket.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://litestream/sync", bytes.NewReader(body))
	if err != nil {
		return Position{}, fmt.Errorf("litestream: sync: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Position{}, fmt.Errorf("litestream: sync: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Litestream's reason, such as {"error":"database not found: ..."}.
		reason, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if err != nil {
			return Position{}, fmt.Errorf("litestream: sync: status %d: reading reason: %w", resp.StatusCode, err)
		}
		return Position{}, fmt.Errorf("litestream: sync: status %d: %s", resp.StatusCode, bytes.TrimSpace(reason))
	}
	var pos Position
	if err := json.NewDecoder(resp.Body).Decode(&pos); err != nil {
		return Position{}, fmt.Errorf("litestream: sync: decoding response: %w", err)
	}
	return pos, nil
}
