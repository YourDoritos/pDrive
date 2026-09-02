package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/YourDoritos/pdrive/internal/mirror"
)

// Serve accepts client connections on the daemon's unix socket.
//
// The socket is mode 0600 in $XDG_RUNTIME_DIR: it grants full control of the
// user's Proton Drive, so no other account on the machine may reach it.
func (d *Daemon) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return err
	}
	// A socket left behind by a crash would otherwise block startup forever.
	if err := removeStaleSocket(socketPath); err != nil {
		return err
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		ln.Close()
		return err
	}

	go func() {
		<-ctx.Done()
		ln.Close()
		os.Remove(socketPath)
	}()

	d.log.Infof("listening on %s", socketPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			d.log.Warnf("accept: %v", err)
			continue
		}
		go d.handleConn(ctx, conn)
	}
}

// removeStaleSocket deletes a socket file that nothing is listening on.
// A live one is left alone, so a second daemon fails to bind rather than
// silently stealing the first one's clients.
func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", path, 300*time.Millisecond)
	if err == nil {
		conn.Close()
		return errors.New("another pdrived is already running")
	}
	return os.Remove(path)
}

func (d *Daemon) handleConn(ctx context.Context, raw net.Conn) {
	defer raw.Close()
	conn := ipc.NewConn(raw)

	for {
		req, err := conn.ReadRequest()
		if err != nil {
			return // client went away
		}
		resp := d.dispatch(ctx, conn, req)
		if resp == nil {
			return // handler took over the connection
		}
		if err := conn.SendResponse(resp); err != nil {
			return
		}
	}
}

// dispatch handles one request. A nil return means the handler has taken
// ownership of the connection.
func (d *Daemon) dispatch(ctx context.Context, conn *ipc.Conn, req *ipc.Request) *ipc.Response {
	switch req.Command {
	case ipc.CmdPing:
		return ipc.OK(nil)

	case ipc.CmdStatus:
		return ipc.OK(d.Status())

	case ipc.CmdSync:
		var p ipc.SyncParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return ipc.Errorf("bad parameters: %v", err)
			}
		}
		res, err := d.RequestSync(ctx, p)
		if err != nil {
			return ipc.Errorf("%v", err)
		}
		return ipc.OK(resultToData(res))

	case ipc.CmdPause:
		d.Pause()
		return ipc.OK(nil)

	case ipc.CmdResume:
		d.Resume()
		return ipc.OK(nil)

	case ipc.CmdConflicts:
		conflicts, err := d.Conflicts()
		if err != nil {
			return ipc.Errorf("%v", err)
		}
		return ipc.OK(ipc.ConflictsData{Conflicts: conflicts})

	case ipc.CmdGet:
		var p ipc.GetParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return ipc.Errorf("bad parameters: %v", err)
		}
		if err := d.Materialize(ctx, p.Path); err != nil {
			return ipc.Errorf("%v", err)
		}
		return ipc.OK(nil)

	default:
		return ipc.Errorf("unknown command %q", req.Command)
	}
}

func resultToData(res *mirror.Result) ipc.SyncData {
	if res == nil {
		return ipc.SyncData{}
	}
	return ipc.SyncData{
		Downloaded: res.Downloaded, Uploaded: res.Uploaded, Moved: res.Moved,
		TrashedRemote: res.TrashedRemote, Deleted: res.Deleted, Stubbed: res.Stubbed,
		Conflicts: res.Conflicts, Warnings: res.Warnings,
		Bytes: res.Bytes, UploadedBytes: res.UploadedBytes, FullMirror: res.FullMirror,
	}
}
