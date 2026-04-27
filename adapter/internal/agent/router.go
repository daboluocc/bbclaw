// Router multiplexes multiple agent.Driver implementations behind a single
// lookup surface. The HTTP layer owns one Router and resolves per-request
// drivers either by explicit name or by falling back to the default.
//
// Thread-safety: Register is expected to run during startup, but Get /
// Default / List are hot-path lookups done under an RWMutex so concurrent
// requests do not serialise on each other.
package agent

import (
	"sync"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
)

// DriverInfo is the public shape returned by Router.List. It is also what
// the GET /v1/agent/drivers HTTP endpoint serialises to the client.
type DriverInfo struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
}

// Router keeps a name -> Driver map plus a pointer to the default driver
// (the first one registered). Drivers are looked up by their Name().
type Router struct {
	mu          sync.RWMutex
	drivers     map[string]Driver
	defaultName string
}

// NewRouter returns an empty Router. Call Register for each driver you want
// to expose; the first successful Register becomes the default.
func NewRouter() *Router {
	return &Router{drivers: make(map[string]Driver)}
}

// Register adds a driver to the router keyed by d.Name(). The first driver
// registered becomes the default. A duplicate Name() overwrites the existing
// entry and logs a warning (useful for hot-reload style wiring, but
// unexpected in production).
func (r *Router) Register(d Driver, log *obs.Logger) {
	if d == nil {
		return
	}
	name := d.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.drivers[name]; exists && log != nil {
		log.Warnf("agent router: driver %q re-registered, overwriting previous entry", name)
	}
	r.drivers[name] = d
	if r.defaultName == "" {
		r.defaultName = name
	}
}

// Get returns the driver registered under name and whether it was found.
func (r *Router) Get(name string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	return d, ok
}

// Default returns the driver that was registered first, or nil if the
// router is empty.
func (r *Router) Default() Driver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.defaultName == "" {
		return nil
	}
	return r.drivers[r.defaultName]
}

// List returns a snapshot of all registered drivers with their capabilities.
// Order is not guaranteed; the caller should sort if stable output matters.
func (r *Router) List() []DriverInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DriverInfo, 0, len(r.drivers))
	for _, d := range r.drivers {
		out = append(out, DriverInfo{Name: d.Name(), Capabilities: d.Capabilities()})
	}
	return out
}

// DefaultName returns the name of the current default driver, or "" if the
// router is empty.
func (r *Router) DefaultName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultName
}

// SetDefault overrides the default driver. Returns true on success, false if
// name is not a registered driver (in which case the previous default is
// left untouched).
func (r *Router) SetDefault(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drivers[name]; !ok {
		return false
	}
	r.defaultName = name
	return true
}
