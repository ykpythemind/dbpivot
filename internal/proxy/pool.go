package proxy

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/ykpythemind/db-pool-switch/internal/config"
)

// ResolvedTarget is a snapshot of a target with all variables and forward
// references resolved. It is what handlers actually use to dial upstreams.
type ResolvedTarget struct {
	Name     string
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// Conn is a registered (client, upstream) pair guarded by a once-only close.
type Conn struct {
	Client    net.Conn
	Upstream  net.Conn
	Resolved  ResolvedTarget
	closeOnce sync.Once
	pool      *Pool
}

// Close shuts down both ends of the connection and deregisters from the pool.
// Safe to call concurrently and multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		if c.Client != nil {
			_ = c.Client.Close()
		}
		if c.Upstream != nil {
			_ = c.Upstream.Close()
		}
		if c.pool != nil {
			c.pool.deregister(c)
		}
	})
}

// Pool holds the routing state for one logical pool (= one dbname clients
// connect with). Targets and the forward map are immutable after creation
// (use Replace under the Server's coordination to swap them on reload).
type Pool struct {
	name    string
	targets []config.Target
	byName  map[string]*config.Target
	fwd     map[string]config.ForwardTarget

	current atomic.Pointer[ResolvedTarget]

	mu    sync.Mutex
	conns map[*Conn]struct{}
}

func NewPool(p config.Pool, fwd map[string]config.ForwardTarget) (*Pool, error) {
	out := &Pool{
		name:    p.Name,
		targets: append([]config.Target(nil), p.Targets...),
		byName:  make(map[string]*config.Target, len(p.Targets)),
		fwd:     fwd,
		conns:   make(map[*Conn]struct{}),
	}
	for i := range out.targets {
		out.byName[out.targets[i].Name] = &out.targets[i]
	}

	def := out.byName[p.Default]
	if def == nil {
		return nil, fmt.Errorf("pool %q: default target %q missing", p.Name, p.Default)
	}
	host, port := def.ResolveEndpoint(fwd)
	out.current.Store(&ResolvedTarget{
		Name:     def.Name,
		Host:     host,
		Port:     port,
		User:     def.User,
		Password: def.Password,
		Database: def.Database,
	})
	return out, nil
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.name }

// Current returns the active resolved target.
func (p *Pool) Current() ResolvedTarget { return *p.current.Load() }

// Targets returns the original config Targets (for status/list).
func (p *Pool) Targets() []config.Target { return p.targets }

// ResolveTarget returns the named target's snapshot after substituting
// variables into its database template. It does NOT touch p.current.
func (p *Pool) ResolveTarget(name string, vars map[string]string) (ResolvedTarget, []string, error) {
	t, ok := p.byName[name]
	if !ok {
		return ResolvedTarget{}, nil, fmt.Errorf("unknown target %q for pool %q", name, p.name)
	}
	resolvedDB, missing, err := config.Resolve(t.Database, vars)
	if err != nil && len(missing) > 0 {
		return ResolvedTarget{}, missing, fmt.Errorf("target %q requires variables: %v", name, missing)
	}
	if err != nil {
		return ResolvedTarget{}, nil, err
	}
	if resolvedDB != "" && !config.ValidIdentifier(resolvedDB) {
		return ResolvedTarget{}, nil, fmt.Errorf("resolved database %q contains invalid characters", resolvedDB)
	}
	host, port := t.ResolveEndpoint(p.fwd)
	return ResolvedTarget{
		Name:     t.Name,
		Host:     host,
		Port:     port,
		User:     t.User,
		Password: t.Password,
		Database: resolvedDB,
	}, nil, nil
}

// Switch atomically updates current to the named target (with variables) and
// closes all currently registered connections. Returns the previous resolved
// target and how many connections were closed.
func (p *Pool) Switch(name string, vars map[string]string) (prev ResolvedTarget, closed int, missing []string, err error) {
	rt, miss, err := p.ResolveTarget(name, vars)
	if err != nil {
		return ResolvedTarget{}, 0, miss, err
	}
	prev = *p.current.Load()
	p.current.Store(&rt)
	closed = p.dropAll()
	return prev, closed, nil, nil
}

// Register adds c to the active set under p.mu.
func (p *Pool) Register(c *Conn) {
	c.pool = p
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

func (p *Pool) deregister(c *Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// dropAll force-closes every registered connection. It snapshots the set
// under the lock, then closes outside the lock to avoid deadlocking with the
// deregistration path that also takes p.mu.
func (p *Pool) dropAll() int {
	p.mu.Lock()
	snapshot := make([]*Conn, 0, len(p.conns))
	for c := range p.conns {
		snapshot = append(snapshot, c)
	}
	p.mu.Unlock()

	for _, c := range snapshot {
		c.Close()
	}
	return len(snapshot)
}

// ActiveConns returns the current number of registered connections.
func (p *Pool) ActiveConns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// DropAll exposes dropAll for the Server's shutdown path.
func (p *Pool) DropAll() int { return p.dropAll() }
