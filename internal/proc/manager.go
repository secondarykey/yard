package proc

import (
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	"yard/internal/config"
)

// Manager holds the Procs of all servers and looks up servers by path.
type Manager struct {
	logger *slog.Logger

	mu    sync.RWMutex
	procs map[string]*Proc
	order []string // order in the configuration file
	byLen []*Proc  // sorted by path length, longest first (for longest match)
}

// NewManager creates an empty Manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{logger: logger, procs: map[string]*Proc{}}
}

// Apply applies the configuration. Unchanged servers keep running,
// changed ones are recreated, removed ones are stopped, and new ones are started.
// A changed server that was stopped (from the list, or by exiting normally) stays stopped,
// so that changing a setting such as log does not start it.
func (m *Manager) Apply(servers []config.Server) {
	m.mu.Lock()
	old := m.procs
	next := make(map[string]*Proc, len(servers))
	order := make([]string, 0, len(servers))
	var stop, start []*Proc
	for _, t := range servers {
		order = append(order, t.Name)
		if p, ok := old[t.Name]; ok && reflect.DeepEqual(p.conf, t) {
			next[t.Name] = p
			continue
		}
		stopped := false
		if p, ok := old[t.Name]; ok {
			stop = append(stop, p)
			stopped = p.Status().State == StateStopped
		}
		p := New(t, m.logger)
		next[t.Name] = p
		if !t.Disabled && !stopped {
			start = append(start, p)
		}
	}
	for name, p := range old {
		if _, ok := next[name]; !ok {
			stop = append(stop, p)
		}
	}

	byLen := make([]*Proc, 0, len(next))
	for _, p := range next {
		byLen = append(byLen, p)
	}
	sort.Slice(byLen, func(i, j int) bool { return len(byLen[i].conf.Path) > len(byLen[j].conf.Path) })

	m.procs, m.order, m.byLen = next, order, byLen
	m.mu.Unlock()

	// Old processes may hold the same port or files, so stop them before starting.
	parallel(stop, func(p *Proc) { p.Stop() })
	for _, p := range start {
		if err := p.Start(); err != nil {
			p.logger.Error("start", "err", err)
		}
	}
}

// StopAll stops all servers.
func (m *Manager) StopAll() {
	m.mu.RLock()
	all := make([]*Proc, 0, len(m.procs))
	for _, p := range m.procs {
		all = append(all, p)
	}
	m.mu.RUnlock()
	parallel(all, func(p *Proc) { p.Stop() })
}

// Get returns the Proc with the given name.
func (m *Manager) Get(name string) (*Proc, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.procs[name]
	return p, ok
}

// Statuses returns the states of all servers in configuration file order.
func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.procs[name].Status())
	}
	return out
}

// Match returns the server whose path is the longest match for the URL path.
// "/memo" matches "/memo" and "/memo/...", but not "/memox".
func (m *Manager) Match(urlPath string) (*Proc, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.byLen {
		if MatchPath(p.conf.Path, urlPath) {
			return p, true
		}
	}
	return nil, false
}

// MatchPath reports whether prefix covers urlPath.
func MatchPath(prefix, urlPath string) bool {
	if prefix == "/" {
		return true
	}
	return urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/")
}

func parallel(ps []*Proc, f func(*Proc)) {
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Go(func() { f(p) })
	}
	wg.Wait()
}
