package posadapter

import (
	"fmt"
	"sort"
	"sync"
)

// Factory creates a fresh Adapter instance. Config is passed at Connect
// time, not here — factories are pure so adapters can be instantiated
// before config is parsed (useful for tooling like `fuelmind adapters list`).
type Factory func() Adapter

// Registry holds the set of adapter factories known to this binary.
// Adapters register themselves via Register() in their init() function.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

var defaultRegistry = &Registry{
	factories: make(map[string]Factory),
}

// Register makes an adapter factory available by name. Typically called
// from an adapter package's init() function.
//
// Panics on duplicate registration — that's a programmer error (two
// adapter packages both using the same Name) and must be caught at boot,
// not in production.
func Register(name string, f Factory) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	if _, exists := defaultRegistry.factories[name]; exists {
		panic("posadapter: duplicate registration for adapter " + name)
	}
	defaultRegistry.factories[name] = f
}

// New returns a fresh Adapter instance by name, or an error if no adapter
// is registered under that name.
func New(name string) (Adapter, error) {
	defaultRegistry.mu.RLock()
	f, ok := defaultRegistry.factories[name]
	defaultRegistry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("posadapter: no adapter registered as %q (known: %v)", name, Names())
	}
	return f(), nil
}

// Names returns the registered adapter names in stable (sorted) order.
// Useful for `fuelmind adapters list` and for tests that want to assert
// the set of built-in adapters.
func Names() []string {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	out := make([]string, 0, len(defaultRegistry.factories))
	for k := range defaultRegistry.factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
