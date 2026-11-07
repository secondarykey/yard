package proc

import (
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"

	"yard/internal/config"
)

// Manager は全サーバの Proc を持ち、パスからサーバを引く。
type Manager struct {
	logger *log.Logger

	mu    sync.RWMutex
	procs map[string]*Proc
	order []string // 設定ファイルでの並び
	byLen []*Proc  // path の長い順（最長一致用）
}

// NewManager は空の Manager を作る。
func NewManager(logger *log.Logger) *Manager {
	return &Manager{logger: logger, procs: map[string]*Proc{}}
}

// Apply は設定を反映する。設定が変わっていないサーバは動かしたままにし、
// 変わったものは作り直し、消えたものは止め、増えたものは起動する。
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
		if p, ok := old[t.Name]; ok {
			stop = append(stop, p)
		}
		p := New(t, m.logger)
		next[t.Name] = p
		if !t.Disabled {
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

	// 古いプロセスが同じポートやファイルを掴んでいることがあるので、止めてから起動する。
	parallel(stop, func(p *Proc) { p.Stop() })
	for _, p := range start {
		if err := p.Start(); err != nil {
			m.logger.Printf("[%s] %v", p.conf.Name, err)
		}
	}
}

// StopAll は全サーバを止める。
func (m *Manager) StopAll() {
	m.mu.RLock()
	all := make([]*Proc, 0, len(m.procs))
	for _, p := range m.procs {
		all = append(all, p)
	}
	m.mu.RUnlock()
	parallel(all, func(p *Proc) { p.Stop() })
}

// Get は名前で Proc を返す。
func (m *Manager) Get(name string) (*Proc, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.procs[name]
	return p, ok
}

// Statuses は設定ファイルの並びで全サーバの状態を返す。
func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.procs[name].Status())
	}
	return out
}

// Match は URL パスに最長一致するサーバを返す。
// "/memo" は "/memo" と "/memo/..." に一致し、"/memox" には一致しない。
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

// MatchPath は prefix が urlPath を受け持つかを返す。
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
