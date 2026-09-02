package gate

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Logger receives the gate's diagnostics.
type Logger interface {
	Infof(format string, v ...interface{})
	Warnf(format string, v ...interface{})
	Errorf(format string, v ...interface{})
}

// Server is pdrive-gate.
type Server struct {
	fan      *Fanotify
	log      Logger
	deadline time.Duration

	mu        sync.Mutex
	client    *Conn
	clientPID int
	root      string

	nextID  atomic.Uint64
	pending sync.Map // id -> chan struct{}

	// Stats, for `pdrive-gate --status` and for judging whether the deadline
	// is set sensibly.
	held    atomic.Int64
	timeout atomic.Int64
	allowed atomic.Int64
}

// NewServer creates a gate.
func NewServer(log Logger, deadline time.Duration) (*Server, error) {
	fan, err := OpenFanotify()
	if err != nil {
		return nil, err
	}
	if deadline <= 0 || deadline > time.Second {
		deadline = 400 * time.Millisecond
	}
	return &Server{fan: fan, log: log, deadline: deadline}, nil
}

// Close releases the gate.
func (s *Server) Close() {
	s.fan.UnmarkAll()
	s.fan.Close()
}

// Listen accepts daemon connections on the gate socket.
func (s *Server) Listen(ctx context.Context, socketPath string) error {
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	// World-writable by design: any user's daemon must be able to register
	// its own sync root. Authorisation is by peer credentials below, not by
	// file mode, because the mode cannot express "your own directories only".
	if err := os.Chmod(socketPath, 0666); err != nil {
		ln.Close()
		return err
	}

	go func() {
		<-ctx.Done()
		ln.Close()
		os.Remove(socketPath)
	}()

	s.log.Infof("gate listening on %s (deadline %s)", socketPath, s.deadline)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleClient(ctx, conn)
	}
}

// handleClient services one daemon connection.
func (s *Server) handleClient(ctx context.Context, raw net.Conn) {
	defer raw.Close()

	uid, pid, err := peerCred(raw)
	if err != nil {
		s.log.Warnf("rejecting connection without peer credentials: %v", err)
		return
	}

	conn := NewConn(raw)
	msg, err := conn.Recv()
	if err != nil || msg.Type != TypeRegister {
		s.log.Warnf("first message from pid %d was not a registration", pid)
		return
	}

	root, err := s.authorizeRoot(msg.Root, uid)
	if err != nil {
		s.log.Warnf("refusing to watch %q for uid %d: %v", msg.Root, uid, err)
		return
	}

	s.mu.Lock()
	if s.client != nil {
		s.mu.Unlock()
		s.log.Warnf("a daemon is already registered; refusing pid %d", pid)
		return
	}
	s.client = conn
	s.clientPID = msg.PID
	if s.clientPID == 0 {
		s.clientPID = pid
	}
	s.root = root
	s.mu.Unlock()

	n, _ := s.fan.MarkTree(root)
	s.log.Infof("watching %s for pid %d (%d directories marked)", root, s.clientPID, n)

	defer func() {
		// Fail open. With the daemon gone there is nobody to answer, so every
		// mark is dropped and the filesystem behaves as if no gate existed.
		s.fan.UnmarkAll()
		s.mu.Lock()
		s.client, s.clientPID, s.root = nil, 0, ""
		s.mu.Unlock()
		s.log.Infof("daemon disconnected; all marks released")
	}()

	for {
		msg, err := conn.Recv()
		if err != nil {
			return
		}
		switch msg.Type {
		case TypeAck:
			if ch, ok := s.pending.Load(msg.ID); ok {
				close(ch.(chan struct{}))
				s.pending.Delete(msg.ID)
			}
		case TypeRescan:
			if added, _ := s.fan.MarkTree(s.currentRoot()); added > 0 {
				s.log.Infof("rescanned; %d directories marked", s.fan.Marked())
			}
		}
	}
}

// authorizeRoot checks that the caller owns the directory it wants watched.
//
// Without this, any local user could ask a root process to intercept opens in
// directories belonging to somebody else and stall them. The mode on the
// socket cannot express that constraint; peer credentials can.
func (s *Server) authorizeRoot(root string, uid uint32) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("root must be an absolute path")
	}
	clean := filepath.Clean(root)
	if clean == "/" || !strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("refusing to watch %q", clean)
	}

	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot determine ownership")
	}
	if st.Uid != uid {
		return "", fmt.Errorf("directory is owned by uid %d, not %d", st.Uid, uid)
	}
	return clean, nil
}

func (s *Server) currentRoot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// Run reads permission events and answers them.
func (s *Server) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.fan.Close()
	}()

	buf := make([]byte, 64*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := s.fan.Read(buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		for _, ev := range events {
			go s.decide(ev)
		}
	}
}

// decide holds one listing until the daemon says the directory is current, or
// until the deadline expires.
func (s *Server) decide(ev Event) {
	defer s.fan.Allow(ev)
	s.allowed.Add(1)

	s.mu.Lock()
	client, daemonPID, root := s.client, s.clientPID, s.root
	s.mu.Unlock()

	if client == nil {
		return // nobody to ask
	}
	// The daemon's own reads must never wait on the daemon. This is the
	// classic fanotify self-deadlock and the reason the daemon reports its
	// pid at registration.
	if ev.PID == daemonPID {
		return
	}
	if ev.Path == "" || !underRoot(ev.Path, root) {
		return
	}

	id := s.nextID.Add(1)
	done := make(chan struct{})
	s.pending.Store(id, done)
	defer s.pending.Delete(id)

	if err := client.Send(&Message{Type: TypeOpened, ID: id, Path: ev.Path}); err != nil {
		return
	}

	s.held.Add(1)
	select {
	case <-done:
	case <-time.After(s.deadline):
		// Proton latency must never become filesystem latency. The listing is
		// released possibly one beat stale, which is exactly what the
		// gate-less fallback would have given anyway.
		s.timeout.Add(1)
	}
}

func underRoot(path, root string) bool {
	if root == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// Stats reports what the gate has done.
func (s *Server) Stats() (allowed, held, timedOut int64) {
	return s.allowed.Load(), s.held.Load(), s.timeout.Load()
}

// peerCred returns the uid and pid of the process at the other end.
func peerCred(c net.Conn) (uint32, int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}

	var (
		cred    *unix.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	return cred.Uid, int(cred.Pid), nil
}
