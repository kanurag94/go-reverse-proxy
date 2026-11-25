package watch_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kanurag94/go-reverse-proxy/internal/watch"
)

// save replaces the file the way editors do: write a temp file, then rename it over.
func save(t *testing.T, path, content string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// watching watches path and returns the contents delivered, in order.
func watching(t *testing.T, path string) <-chan string {
	got := make(chan string, 10)
	stop, err := watch.File(path, func(content []byte) { got <- string(content) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return got
}

func next(t *testing.T, got <-chan string) string {
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no change delivered")
		return ""
	}
}

func TestChangesAreDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	save(t, path, "v1")
	got := watching(t, path)

	for _, want := range []string{"v2", "v3"} { // v3 shows the watch survives the file being replaced
		save(t, path, want)
		if s := next(t, got); s != want {
			t.Fatalf("delivered %q, want %q", s, want)
		}
	}
}

func TestUnchangedSaveIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	save(t, path, "v1")
	got := watching(t, path)

	save(t, path, "v2")
	if s := next(t, got); s != "v2" {
		t.Fatalf("delivered %q, want v2", s)
	}
	save(t, path, "v2")
	save(t, path, "v3")
	if s := next(t, got); s != "v3" {
		t.Fatalf("delivered %q, want v3: the unchanged save must not be delivered", s)
	}
}
