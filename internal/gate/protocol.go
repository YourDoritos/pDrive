// Package gate holds pdrive-gate: the small privileged helper that makes a
// directory listing wait until the daemon has caught that directory up.
//
// Proton offers no push channel, so a client that only polls is always some
// interval out of date. The gate closes that window by intercepting
// opendir(2) with fanotify permission events and releasing it once the
// daemon has applied the event-cursor delta for that directory. Measured on
// Linux 7.1.9: the tax on a listing with nothing to do is ~70 microseconds,
// and a killed gate fails open at the kernel level.
//
// Everything here runs as root, so it is deliberately small and does no
// network I/O, no cryptography, and holds no credentials.
package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Message types on the gate socket.
const (
	// TypeRegister is sent by pdrived to claim a sync root.
	TypeRegister = "register"
	// TypeOpened is sent by the gate when a watched directory is opened.
	TypeOpened = "opened"
	// TypeAck is pdrived's answer: this directory is now up to date.
	TypeAck = "ack"
	// TypeRescan asks the gate to refresh its marks after the tree changed.
	TypeRescan = "rescan"
)

// Message is one frame on the gate socket.
type Message struct {
	Type string `json:"type"`

	// Register
	Root string `json:"root,omitempty"`
	PID  int    `json:"pid,omitempty"`

	// Opened / Ack
	ID   uint64 `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
}

// Conn is a framed connection to the gate.
type Conn struct {
	raw net.Conn
	r   *bufio.Reader
}

// NewConn wraps a connection.
func NewConn(c net.Conn) *Conn { return &Conn{raw: c, r: bufio.NewReader(c)} }

// Send writes one message.
func (c *Conn) Send(m *Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := c.raw.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Recv reads one message.
func (c *Conn) Recv() (*Message, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("read gate message: %w", err)
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("parse gate message: %w", err)
	}
	return &m, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.raw.Close() }

// SocketPath is where the gate listens.
const SocketPath = "/run/pdrive-gate.sock"
