// Package watch reports changes to a file's content.
package watch

import (
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// File calls onChange with the file's content each time that content changes. It
// watches the directory, not the file: editors and Kubernetes replace the file
// instead of writing into it, which would end a watch on the file itself.
func File(path string, onChange func(content []byte)) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		w.Close()
		return nil, err
	}
	go func() {
		for err := range w.Errors {
			slog.Warn("file watch", "path", path, "err", err)
		}
	}()
	b, _ := os.ReadFile(path)
	last := sha256.Sum256(b)
	go func() {
		for range w.Events { // any event in the directory; the hash says whether this file changed
			b, err := os.ReadFile(path)
			if err != nil {
				continue // gone for now; the event that brings it back reads it again
			}
			if h := sha256.Sum256(b); h != last {
				last = h
				onChange(b)
			}
		}
	}()
	return func() { w.Close() }, nil
}
