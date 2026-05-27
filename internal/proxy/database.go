package proxy

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/ykpythemind/dbpivot/internal/config"
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
	SSLMode  string
}

// Conn is a registered (client, upstream) pair guarded by a once-only close.
type Conn struct {
	Client    net.Conn
	Upstream  net.Conn
	Resolved  ResolvedTarget
	closeOnce sync.Once
	database  *Database
}

// Close shuts down both ends of the connection and deregisters from the database.
// Safe to call concurrently and multiple times.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		if c.Client != nil {
			_ = c.Client.Close()
		}
		if c.Upstream != nil {
			_ = c.Upstream.Close()
		}
		if c.database != nil {
			c.database.deregister(c)
		}
	})
}

// Database holds the routing state for one logical database (= one dbname
// clients connect with). The current target is unset until Apply is called;
// Server activates all databases before accepting traffic.
type Database struct {
	virtualName string
	targets     []config.Target
	byName      map[string]*config.Target
	fwd         map[string]config.ForwardTarget

	current atomic.Pointer[ResolvedTarget]

	mu    sync.Mutex
	conns map[*Conn]struct{}
}

func NewDatabase(c config.Database, fwd map[string]config.ForwardTarget) *Database {
	out := &Database{
		virtualName: c.VirtualName,
		targets:     append([]config.Target(nil), c.Targets...),
		byName:      make(map[string]*config.Target, len(c.Targets)),
		fwd:         fwd,
		conns:       make(map[*Conn]struct{}),
	}
	for i := range out.targets {
		out.byName[out.targets[i].Name] = &out.targets[i]
	}
	return out
}

// VirtualName returns the configured virtual_name (= the dbname clients connect with).
func (d *Database) VirtualName() string { return d.virtualName }

// Current returns the active resolved target. Returns the zero value and
// false if the database has not been activated yet.
func (d *Database) Current() (ResolvedTarget, bool) {
	p := d.current.Load()
	if p == nil {
		return ResolvedTarget{}, false
	}
	return *p, true
}

// Targets returns the original config Targets (for status/list).
func (d *Database) Targets() []config.Target { return d.targets }

// HasTarget reports whether this database declares a target with the given name.
func (d *Database) HasTarget(name string) bool {
	_, ok := d.byName[name]
	return ok
}

// ResolveTarget returns the named target's snapshot after substituting
// variables into its database template. It does NOT touch d.current — this
// is the planning phase of a switch.
func (d *Database) ResolveTarget(name string, vars map[string]string) (ResolvedTarget, []string, error) {
	t, ok := d.byName[name]
	if !ok {
		return ResolvedTarget{}, nil, fmt.Errorf("unknown target %q for database %q", name, d.virtualName)
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
	host, port := t.ResolveEndpoint(d.fwd)
	return ResolvedTarget{
		Name:     t.Name,
		Host:     host,
		Port:     port,
		User:     t.User,
		Password: t.Password,
		Database: resolvedDB,
		SSLMode:  t.SSLMode,
	}, nil, nil
}

// Apply atomically swaps in rt as the current target and force-closes every
// previously-registered connection. Returns the previous target (nil if this
// is the first activation) and how many connections were closed.
func (d *Database) Apply(rt ResolvedTarget) (prev *ResolvedTarget, closed int) {
	prev = d.current.Load()
	d.current.Store(&rt)
	closed = d.dropAll()
	return prev, closed
}

// Clear atomically removes the current target and force-closes every
// previously-registered connection. Used when a global `use <target>`
// targets a name this database doesn't declare — the database becomes
// inactive and new client connections receive a clean PG error.
func (d *Database) Clear() (prev *ResolvedTarget, closed int) {
	prev = d.current.Swap(nil)
	closed = d.dropAll()
	return prev, closed
}

// Register adds c to the active set under d.mu.
func (d *Database) Register(c *Conn) {
	c.database = d
	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
}

func (d *Database) deregister(c *Conn) {
	d.mu.Lock()
	delete(d.conns, c)
	d.mu.Unlock()
}

// dropAll force-closes every registered connection. It snapshots the set
// under the lock, then closes outside the lock to avoid deadlocking with the
// deregistration path that also takes d.mu.
func (d *Database) dropAll() int {
	d.mu.Lock()
	snapshot := make([]*Conn, 0, len(d.conns))
	for c := range d.conns {
		snapshot = append(snapshot, c)
	}
	d.mu.Unlock()

	for _, c := range snapshot {
		c.Close()
	}
	return len(snapshot)
}

// ActiveConns returns the current number of registered connections.
func (d *Database) ActiveConns() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

// DropAll exposes dropAll for the Server's shutdown path.
func (d *Database) DropAll() int { return d.dropAll() }
