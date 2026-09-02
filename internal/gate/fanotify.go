package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Fanotify holds the permission-event file descriptor and its marks.
type Fanotify struct {
	fd int

	mu     sync.Mutex
	marked map[string]bool
}

// OpenFanotify creates a permission-event fanotify group.
//
// FAN_CLASS_CONTENT is the class that permits FAN_OPEN_PERM. It requires
// CAP_SYS_ADMIN, which is the entire reason this component is separate from
// the daemon.
func OpenFanotify() (*Fanotify, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_CONTENT|unix.FAN_CLOEXEC, unix.O_RDONLY|unix.O_LARGEFILE)
	if err != nil {
		if err == unix.EPERM {
			return nil, fmt.Errorf("fanotify requires CAP_SYS_ADMIN (run pdrive-gate as root): %w", err)
		}
		return nil, fmt.Errorf("fanotify_init: %w", err)
	}
	return &Fanotify{fd: fd, marked: map[string]bool{}}, nil
}

// Close releases the fanotify group.
//
// Closing the descriptor makes the kernel allow every permission event still
// outstanding, which is why a crashed or killed gate cannot wedge the
// filesystem. Verified by hand: SIGKILL during a held listing released it
// immediately with exit status 0.
func (f *Fanotify) Close() error { return unix.Close(f.fd) }

// Mark watches one directory for open-permission events.
//
// Marks are per-inode and added only under the sync root. FAN_MARK_FILESYSTEM
// would be far cheaper to maintain and is never used: it would put every open
// on the whole filesystem through this process.
func (f *Fanotify) Mark(dir string) error {
	f.mu.Lock()
	if f.marked[dir] {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()

	err := unix.FanotifyMark(f.fd, unix.FAN_MARK_ADD,
		unix.FAN_OPEN_PERM|unix.FAN_ONDIR, unix.AT_FDCWD, dir)
	if err != nil {
		return fmt.Errorf("mark %s: %w", dir, err)
	}

	f.mu.Lock()
	f.marked[dir] = true
	f.mu.Unlock()
	return nil
}

// Unmark stops watching a directory.
func (f *Fanotify) Unmark(dir string) {
	err := unix.FanotifyMark(f.fd, unix.FAN_MARK_REMOVE,
		unix.FAN_OPEN_PERM|unix.FAN_ONDIR, unix.AT_FDCWD, dir)
	_ = err

	f.mu.Lock()
	delete(f.marked, dir)
	f.mu.Unlock()
}

// UnmarkAll drops every mark, releasing the tree entirely.
//
// Called whenever the daemon goes away or misbehaves: with no marks, nothing
// is intercepted and the filesystem behaves exactly as if the gate were not
// installed.
func (f *Fanotify) UnmarkAll() {
	f.mu.Lock()
	dirs := make([]string, 0, len(f.marked))
	for d := range f.marked {
		dirs = append(dirs, d)
	}
	f.mu.Unlock()

	for _, d := range dirs {
		f.Unmark(d)
	}
}

// MarkTree marks a directory and everything beneath it.
func (f *Fanotify) MarkTree(root string) (int, error) {
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is skipped, not fatal
		}
		if base := filepath.Base(path); path != root && (base == ".pdrive-tmp") {
			return filepath.SkipDir
		}
		if markErr := f.Mark(path); markErr == nil {
			count++
		}
		return nil
	})
	return count, err
}

// Marked returns how many directories are currently marked.
func (f *Fanotify) Marked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.marked)
}

// Event is one open-permission event awaiting a verdict.
type Event struct {
	Fd   int
	PID  int
	Path string
}

// Read blocks until at least one event arrives.
//
// The caller MUST answer every returned event, with Allow, and MUST close its
// Fd. An unanswered event leaves the opening process blocked until this
// process exits.
func (f *Fanotify) Read(buf []byte) ([]Event, error) {
	n, err := unix.Read(f.fd, buf)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, nil
	}

	var events []Event
	for offset := 0; offset+int(unsafe.Sizeof(unix.FanotifyEventMetadata{})) <= n; {
		meta := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[offset]))
		if meta.Event_len == 0 || offset+int(meta.Event_len) > n {
			break
		}
		offset += int(meta.Event_len)

		if meta.Mask&unix.FAN_OPEN_PERM == 0 {
			if meta.Fd >= 0 {
				unix.Close(int(meta.Fd))
			}
			continue
		}

		ev := Event{Fd: int(meta.Fd), PID: int(meta.Pid)}
		if meta.Fd >= 0 {
			if target, lerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", meta.Fd)); lerr == nil {
				ev.Path = target
			}
		}
		events = append(events, ev)
	}
	return events, nil
}

// Allow releases a held event and closes its descriptor.
//
// The gate never denies an open. It has no reason to refuse access to a
// user's own files, and a bug that denied one would look to the user like
// filesystem corruption.
func (f *Fanotify) Allow(ev Event) {
	if ev.Fd < 0 {
		return
	}
	resp := unix.FanotifyResponse{Fd: int32(ev.Fd), Response: unix.FAN_ALLOW}
	buf := (*[unsafe.Sizeof(resp)]byte)(unsafe.Pointer(&resp))[:]
	_, _ = unix.Write(f.fd, buf)
	unix.Close(ev.Fd)
}
