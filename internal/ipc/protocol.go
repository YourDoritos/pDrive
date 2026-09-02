// Package ipc is the newline-delimited JSON protocol between pdrived and its
// clients (the TUI and pdrivectl). Ported from pVPN.
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Commands understood by the daemon.
const (
	CmdStatus    = "status"
	CmdSync      = "sync"
	CmdPause     = "pause"
	CmdResume    = "resume"
	CmdConflicts = "conflicts"
	CmdGet       = "get"
	CmdPing      = "ping"
	// CmdNotifyOpen is how pdrive-gate reports that a synced directory is
	// being listed. The daemon answers when that directory is fresh.
	CmdNotifyOpen = "notify-open"
)

// Request is a command from a client to the daemon.
type Request struct {
	Command string          `json:"command"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Event is an unsolicited push from the daemon.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Event types.
const (
	EventSyncStarted  = "sync-started"
	EventSyncFinished = "sync-finished"
	EventActivity     = "activity"
)

// --- parameters ---

// SyncParams asks for a sync pass.
type SyncParams struct {
	Full             bool `json:"full,omitempty"`
	DownOnly         bool `json:"down_only,omitempty"`
	ConfirmDeletions bool `json:"confirm_deletions,omitempty"`
}

// GetParams asks for a stubbed file to be materialized.
type GetParams struct {
	Path string `json:"path"`
}

// NotifyOpenParams reports a directory listing in progress.
type NotifyOpenParams struct {
	Path string `json:"path"`
}

// --- response data ---

// StatusData describes the daemon's current state.
type StatusData struct {
	State      string `json:"state"` // idle | syncing | paused | error
	Account    string `json:"account,omitempty"`
	Root       string `json:"root"`
	Files      int    `json:"files"`
	Dirs       int    `json:"dirs"`
	Stubs      int    `json:"stubs"`
	Bytes      int64  `json:"bytes"`
	OnDisk     int64  `json:"on_disk"`
	Conflicts  int    `json:"conflicts"`
	LastSync   string `json:"last_sync,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	GateActive bool   `json:"gate_active"`
	Uptime     string `json:"uptime,omitempty"`
	Pending    int    `json:"pending,omitempty"`
}

// SyncData summarises a completed pass.
type SyncData struct {
	Downloaded    int   `json:"downloaded"`
	Uploaded      int   `json:"uploaded"`
	Moved         int   `json:"moved"`
	TrashedRemote int   `json:"trashed_remote"`
	Deleted       int   `json:"deleted"`
	Stubbed       int   `json:"stubbed"`
	Conflicts     int   `json:"conflicts"`
	Warnings      int   `json:"warnings"`
	Bytes         int64 `json:"bytes"`
	UploadedBytes int64 `json:"uploaded_bytes"`
	FullMirror    bool  `json:"full_mirror"`
}

// ConflictEntry is one preserved local copy.
type ConflictEntry struct {
	Path      string `json:"path"`
	KeptLocal string `json:"kept_local"`
	At        string `json:"at,omitempty"`
}

// ConflictsData lists preserved local copies.
type ConflictsData struct {
	Conflicts []ConflictEntry `json:"conflicts"`
}

// ActivityData is a line of progress.
type ActivityData struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
}

// --- wire helpers ---

// WriteJSON writes one newline-delimited JSON message.
func WriteJSON(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeAll(w, append(data, '\n'))
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// ReadJSON reads one newline-delimited JSON message.
func ReadJSON(r *bufio.Reader, v interface{}) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Conn wraps a net.Conn with buffered reads and JSON helpers.
type Conn struct {
	Raw    net.Conn
	Reader *bufio.Reader
}

// NewConn wraps a connection.
func NewConn(c net.Conn) *Conn {
	return &Conn{Raw: c, Reader: bufio.NewReader(c)}
}

// SendRequest sends a request.
func (c *Conn) SendRequest(req *Request) error { return WriteJSON(c.Raw, req) }

// SendResponse sends a response.
func (c *Conn) SendResponse(resp *Response) error { return WriteJSON(c.Raw, resp) }

// SendEvent sends an event.
func (c *Conn) SendEvent(evt *Event) error { return WriteJSON(c.Raw, evt) }

// ReadRequest reads a request.
func (c *Conn) ReadRequest() (*Request, error) {
	var req Request
	if err := ReadJSON(c.Reader, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ReadResponse reads a response.
func (c *Conn) ReadResponse() (*Response, error) {
	var resp Response
	if err := ReadJSON(c.Reader, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.Raw.Close() }

// MarshalData marshals a value for a Response or Event payload.
func MarshalData(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// Errorf builds a failed response.
func Errorf(format string, args ...interface{}) *Response {
	return &Response{OK: false, Error: fmt.Sprintf(format, args...)}
}

// OK builds a successful response carrying data.
func OK(v interface{}) *Response {
	if v == nil {
		return &Response{OK: true}
	}
	return &Response{OK: true, Data: MarshalData(v)}
}
