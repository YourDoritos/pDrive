package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/YourDoritos/pdrive/internal/mirror"
)

// watchMask is what the sync folder is watched for.
//
// Raw inotify rather than a wrapper library because of IN_OPEN: listing a
// directory is the "someone is looking at this" signal the fallback freshness
// tier depends on, and no Go wrapper exposes it. Verified on this kernel that
// `ls` and file managers produce IN_OPEN|IN_ISDIR while `cd` and `stat`
// produce nothing.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_DELETE_SELF |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_MOVE_SELF |
	unix.IN_CLOSE_WRITE | unix.IN_OPEN

// Watcher reports local activity in the sync folder.
type Watcher struct {
	fd   int
	root string
	log  *Logger

	mu    sync.Mutex
	paths map[int]string // watch descriptor -> directory path

	// onChange fires after a burst of writes settles.
	onChange func()
	// onListing fires when a synced directory is listed.
	onListing func()

	debounce time.Duration
}

// NewWatcher starts watching the sync folder.
func NewWatcher(root string, log *Logger, onChange, onListing func()) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		fd: fd, root: root, log: log,
		paths:    map[int]string{},
		onChange: onChange, onListing: onListing,
		debounce: 750 * time.Millisecond,
	}
	if err := w.addTree(root); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return w, nil
}

// Close stops the watcher.
func (w *Watcher) Close() error { return unix.Close(w.fd) }

// Watched returns how many directories are being watched.
func (w *Watcher) Watched() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.paths)
}

// addTree watches dir and everything beneath it.
func (w *Watcher) addTree(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort
		}
		if !info.IsDir() {
			return nil
		}
		if path != dir && mirror.Ignored(filepath.Base(path)) {
			return filepath.SkipDir
		}
		w.addOne(path)
		return nil
	})
}

func (w *Watcher) addOne(dir string) {
	wd, err := unix.InotifyAddWatch(w.fd, dir, watchMask)
	if err != nil {
		w.log.Debugf("watch %s: %v", dir, err)
		return
	}
	w.mu.Lock()
	w.paths[wd] = dir
	w.mu.Unlock()
}

func (w *Watcher) pathFor(wd int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paths[wd]
}

func (w *Watcher) forget(wd int) {
	w.mu.Lock()
	delete(w.paths, wd)
	w.mu.Unlock()
}

// Run reads events until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		w.Close() // unblocks the read below
	}()

	var (
		changeTimer *time.Timer
		changeC     <-chan time.Time
		lastListing time.Time
	)

	buf := make([]byte, 64*1024)
	events := make(chan localEvent, 256)

	go w.readLoop(buf, events)

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.listing {
				// Rate-limit the attention hint: a file manager can open the
				// same directory many times a second.
				if time.Since(lastListing) > time.Second {
					lastListing = time.Now()
					w.onListing()
				}
				continue
			}
			// Coalesce a burst of writes into a single sync. Saving a file in
			// an editor is several events; a build is thousands.
			if changeTimer == nil {
				changeTimer = time.NewTimer(w.debounce)
				changeC = changeTimer.C
			} else {
				if !changeTimer.Stop() {
					select {
					case <-changeTimer.C:
					default:
					}
				}
				changeTimer.Reset(w.debounce)
			}

		case <-changeC:
			changeTimer, changeC = nil, nil
			w.onChange()
		}
	}
}

type localEvent struct {
	listing bool
}

func (w *Watcher) readLoop(buf []byte, out chan<- localEvent) {
	defer close(out)

	for {
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return // closed
		}
		if n <= 0 {
			return
		}

		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameLen := int(raw.Len)
			name := ""
			if nameLen > 0 {
				start := offset + unix.SizeofInotifyEvent
				name = strings.TrimRight(string(buf[start:start+nameLen]), "\x00")
			}
			offset += unix.SizeofInotifyEvent + nameLen

			w.handle(int(raw.Wd), raw.Mask, name, out)
		}
	}
}

func (w *Watcher) handle(wd int, mask uint32, name string, out chan<- localEvent) {
	isDir := mask&unix.IN_ISDIR != 0

	// A directory being opened is the freshness signal, not a change.
	if mask&unix.IN_OPEN != 0 {
		if isDir {
			select {
			case out <- localEvent{listing: true}:
			default:
			}
		}
		return
	}

	if mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_IGNORED) != 0 {
		w.forget(wd)
		return
	}

	// pdrive's own scratch and stub files must not trigger syncs, or every
	// download would provoke another pass.
	if name != "" && mirror.Ignored(name) {
		return
	}

	// A new directory needs its own watch, and anything already created
	// inside it before the watch landed needs picking up.
	if isDir && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
		if parent := w.pathFor(wd); parent != "" {
			_ = w.addTree(filepath.Join(parent, name))
		}
	}

	select {
	case out <- localEvent{}:
	default: // the debounce will fire regardless; dropping is harmless
	}
}
