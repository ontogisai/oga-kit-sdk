package auth

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// CredentialWatcher monitors a credentials file for changes and invokes a
// callback when the file is modified. Used for event stream credential rotation.
type CredentialWatcher struct {
	path     string
	callback func(path string)
	watcher  *fsnotify.Watcher

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewCredentialWatcher creates a watcher that monitors the given file path.
// When the file changes, the callback is invoked with the path.
func NewCredentialWatcher(path string, callback func(path string)) (*CredentialWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := w.Add(path); err != nil {
		_ = w.Close()
		return nil, err
	}

	cw := &CredentialWatcher{
		path:     path,
		callback: callback,
		watcher:  w,
		stopCh:   make(chan struct{}),
	}

	return cw, nil
}

// Start begins watching for file changes. Call Stop to terminate.
func (cw *CredentialWatcher) Start(_ context.Context) {
	cw.wg.Add(1)
	go cw.loop()
}

// Stop terminates the watcher.
func (cw *CredentialWatcher) Stop() {
	close(cw.stopCh)
	_ = cw.watcher.Close()
	cw.wg.Wait()
}

func (cw *CredentialWatcher) loop() {
	defer cw.wg.Done()

	for {
		select {
		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				slog.Info("credential file changed", "path", cw.path, "op", event.Op)
				cw.callback(cw.path)
			}
		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("credential watcher error", "error", err, "path", cw.path)
		case <-cw.stopCh:
			return
		}
	}
}
