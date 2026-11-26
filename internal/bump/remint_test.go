package bump

import (
	"testing"
	"time"
)

func TestLeafIsRemintedNearExpiry(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca.now = func() time.Time { return now }
	first, _ := ca.leaf("api.example.test")
	now = now.Add(leafLifetime - renewBefore + time.Minute) // inside the renewal window
	if second, _ := ca.leaf("api.example.test"); second == first {
		t.Error("a leaf about to expire was reused")
	}
}
