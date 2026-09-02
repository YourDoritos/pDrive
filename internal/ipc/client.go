package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client is a connection to pdrived.
type Client struct {
	conn *Conn
}

// Dial connects to the daemon. A refused connection means it is not running,
// which callers treat as "fall back to doing the work here" rather than as an
// error.
func Dial(socketPath string) (*Client, error) {
	raw, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &Client{conn: NewConn(raw)}, nil
}

// Available reports whether a daemon is listening.
func Available(socketPath string) bool {
	c, err := Dial(socketPath)
	if err != nil {
		return false
	}
	defer c.Close()
	return c.Ping() == nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// call sends a request and returns the response payload.
func (c *Client) call(cmd string, params interface{}, out interface{}) error {
	req := &Request{Command: cmd}
	if params != nil {
		req.Params = MarshalData(params)
	}
	if err := c.conn.SendRequest(req); err != nil {
		return err
	}

	resp, err := c.conn.ReadResponse()
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// Ping checks the daemon is responding.
func (c *Client) Ping() error { return c.call(CmdPing, nil, nil) }

// Status fetches the daemon's status.
func (c *Client) Status() (*StatusData, error) {
	var out StatusData
	if err := c.call(CmdStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Sync asks the daemon to run a pass and waits for it.
func (c *Client) Sync(p SyncParams) (*SyncData, error) {
	var out SyncData
	if err := c.call(CmdSync, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Pause stops automatic syncing.
func (c *Client) Pause() error { return c.call(CmdPause, nil, nil) }

// Resume restarts automatic syncing.
func (c *Client) Resume() error { return c.call(CmdResume, nil, nil) }

// Conflicts lists preserved local copies.
func (c *Client) Conflicts() ([]ConflictEntry, error) {
	var out ConflictsData
	if err := c.call(CmdConflicts, nil, &out); err != nil {
		return nil, err
	}
	return out.Conflicts, nil
}

// Get materializes a stubbed file.
func (c *Client) Get(path string) error {
	return c.call(CmdGet, GetParams{Path: path}, nil)
}
