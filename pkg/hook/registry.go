package hook

import "sync"

var (
	mu       sync.Mutex
	registry = make(map[string]ConfigFactory)
)

// Register makes a plugin available under name. It panics if name is taken.
func Register(name string, f ConfigFactory) {
	mu.Lock()
	defer mu.Unlock()
	if _, taken := registry[name]; taken {
		panic("hook: " + name + " is registered twice")
	}
	registry[name] = f
}

// Lookup returns the plugin registered under name.
func Lookup(name string) (ConfigFactory, bool) {
	mu.Lock()
	defer mu.Unlock()
	f, ok := registry[name]
	return f, ok
}
