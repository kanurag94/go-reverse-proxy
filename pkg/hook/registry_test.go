package hook_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/kanurag94/go-reverse-proxy/pkg/hook"
)

type nopConfig struct{}

// unique avoids duplicate registrations when tests run more than once.
var runs atomic.Int64

func unique(name string) string { return fmt.Sprintf("%s-%d", name, runs.Add(1)) }

func (nopConfig) Create(string, []byte) (hook.FilterFactory, error) { return nil, nil }

func TestRegisterAndLookup(t *testing.T) {
	name := unique("test-plugin")
	hook.Register(name, nopConfig{})
	if _, ok := hook.Lookup(name); !ok {
		t.Error("a registered plugin was not found")
	}
	if _, ok := hook.Lookup("never-registered"); ok {
		t.Error("found a plugin nobody registered")
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	name := unique("twice")
	hook.Register(name, nopConfig{})
	defer func() {
		if recover() == nil {
			t.Error("a second registration under the same name did not panic")
		}
	}()
	hook.Register(name, nopConfig{})
}
