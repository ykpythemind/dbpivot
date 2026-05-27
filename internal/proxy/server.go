package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ykpythemind/dbpivot/internal/config"
)

const dialTimeout = 5 * time.Second

// SwitchResult captures one database's outcome in a SwitchAll call.
// When Skipped is true, the database does not declare the requested target;
// Current / CurrentDatabase are empty and the database is now inactive (any
// client trying to connect to it receives a PG ErrorResponse until the next
// `use <target>` lands on a target it does declare).
type SwitchResult struct {
	VirtualName      string
	Previous         string // empty if first activation
	PreviousDatabase string
	Current          string
	CurrentDatabase  string
	ClosedConns      int
	Skipped          bool
}

// Server hosts the single TCP listener and the per-database routing state.
// All databases share a single global target name and variable set.
type Server struct {
	addr   string
	logger *slog.Logger

	// adapter is the wire protocol this server speaks on its single listen
	// port. All databases in a config share one adapter (enforced by
	// config.Validate), so the listener can commit to one handshake style.
	adapter string

	// connSeq hands out monotonically increasing per-connection ids, used as
	// the MySQL connection id advertised in the greeting.
	connSeq atomic.Uint32

	listener net.Listener

	mu            sync.RWMutex
	databases     map[string]*Database
	cfg           *config.Config
	currentTarget string
	currentVars   map[string]string
	closed        bool
	wg            sync.WaitGroup
}

// New constructs a Server and activates every database with the given target
// and variables. Fails atomically: if any database cannot resolve the target
// (unknown name, missing variables, invalid resolved database, etc.) no
// databases are activated.
func New(cfg *config.Config, target string, vars map[string]string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	databases := make(map[string]*Database, len(cfg.Databases))
	for _, c := range cfg.Databases {
		databases[c.VirtualName] = NewDatabase(c, cfg.ForwardTargets)
	}
	adapter := config.AdapterPostgres
	if len(cfg.Databases) > 0 && cfg.Databases[0].Adapter != "" {
		adapter = cfg.Databases[0].Adapter
	}
	s := &Server{
		addr:        net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)),
		adapter:     adapter,
		databases:   databases,
		cfg:         cfg,
		currentVars: cloneVars(vars),
		logger:      logger,
	}
	if _, err := s.SwitchAll(target, vars); err != nil {
		return nil, err
	}
	return s, nil
}

// Databases returns a snapshot of the live databases keyed by name.
func (s *Server) Databases() map[string]*Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*Database, len(s.databases))
	for k, v := range s.databases {
		out[k] = v
	}
	return out
}

func (s *Server) lookupDatabase(name string) *Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.databases[name]
}

// CurrentTarget reports the active global target name and the variables it
// was activated with. The returned variable map is a clone.
func (s *Server) CurrentTarget() (string, map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentTarget, cloneVars(s.currentVars)
}

// Addr returns the listen address.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// IsClosed reports whether Shutdown has been initiated.
func (s *Server) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// SwitchAll switches every database to the named target. Databases that do
// not declare the target are warned-and-cleared (their current target is
// removed; future client connections receive a clean PG error until a
// subsequent `use` lands them on a target they declare). Databases that DO
// declare the target are still resolved atomically — if any of them fail to
// resolve (missing variables, invalid identifier, ...) the whole operation
// returns without mutating state. If NO database declares the target, the
// call errors out (almost certainly a typo).
func (s *Server) SwitchAll(target string, vars map[string]string) ([]SwitchResult, error) {
	s.mu.RLock()
	databases := make(map[string]*Database, len(s.databases))
	for k, v := range s.databases {
		databases[k] = v
	}
	s.mu.RUnlock()

	// Phase 1: classify + resolve. Databases lacking the target are
	// recorded as "skip"; the rest must resolve cleanly.
	type plan struct {
		rt   ResolvedTarget
		skip bool
	}
	plans := make(map[string]plan, len(databases))
	declared := 0
	for name, d := range databases {
		if !d.HasTarget(target) {
			plans[name] = plan{skip: true}
			s.logger.Warn("database does not declare requested target; clearing its current target",
				"virtual_name", name, "target", target)
			continue
		}
		declared++
		rt, missing, err := d.ResolveTarget(target, vars)
		if err != nil {
			return nil, &SwitchPlanError{VirtualName: name, Missing: missing, Err: err}
		}
		plans[name] = plan{rt: rt}
	}
	if declared == 0 {
		return nil, fmt.Errorf("no databases declare target %q", target)
	}

	// Phase 2: commit.
	results := make([]SwitchResult, 0, len(databases))
	for name, d := range databases {
		p := plans[name]
		var prev *ResolvedTarget
		var closed int
		r := SwitchResult{VirtualName: name}
		if p.skip {
			prev, closed = d.Clear()
			r.Skipped = true
		} else {
			prev, closed = d.Apply(p.rt)
			r.Current = p.rt.Name
			r.CurrentDatabase = p.rt.Database
		}
		r.ClosedConns = closed
		if prev != nil {
			r.Previous = prev.Name
			r.PreviousDatabase = prev.Database
		}
		results = append(results, r)
	}

	s.mu.Lock()
	s.currentTarget = target
	s.currentVars = cloneVars(vars)
	s.mu.Unlock()
	return results, nil
}

// SwitchPlanError carries a per-database resolution failure from SwitchAll.
type SwitchPlanError struct {
	VirtualName string
	Missing     []string
	Err         error
}

func (e *SwitchPlanError) Error() string {
	return fmt.Sprintf("database %q: %v", e.VirtualName, e.Err)
}

// Start binds the listener and runs the accept loop. It returns once the
// listener has stopped accepting (typically because of Shutdown).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.logger.Info("listening", "addr", ln.Addr(), "target", s.currentTarget)
	for name, d := range s.Databases() {
		if c, ok := d.Current(); ok {
			s.logger.Info("database ready", "virtual_name", name, "target", c.Name, "database", c.Database, "upstream", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
		}
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.IsClosed() {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops accepting, drops all active connections, and waits up to
// ctx's deadline for handler goroutines to exit.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	databases := s.databases
	ln := s.listener
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, d := range databases {
		d.DropAll()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

func (s *Server) handleConn(client net.Conn) {
	defer client.Close()
	var err error
	switch s.adapter {
	case config.AdapterMySQL:
		err = s.dispatchMySQL(client)
	default:
		err = s.dispatch(client)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		s.logger.Warn("conn closed", "remote", client.RemoteAddr(), "err", err)
	}
}

func (s *Server) dispatch(client net.Conn) error {
	br := bufio.NewReader(client)
	for attempt := 0; attempt < 2; attempt++ {
		msgLen, err := ReadUint32(br)
		if err != nil {
			return fmt.Errorf("read length: %w", err)
		}
		if msgLen < 8 || msgLen > MaxStartupLen {
			return fmt.Errorf("startup length out of range: %d", msgLen)
		}
		code, err := ReadUint32(br)
		if err != nil {
			return fmt.Errorf("read code: %w", err)
		}
		switch code {
		case SSLRequestCode, GSSENCRequestCode:
			if _, err := client.Write([]byte{'N'}); err != nil {
				return fmt.Errorf("write SSL/GSS rejection: %w", err)
			}
			continue
		case CancelRequestCode:
			if _, err := io.CopyN(io.Discard, br, int64(msgLen)-8); err != nil {
				return err
			}
			s.logger.Debug("dropped CancelRequest")
			return nil
		case ProtocolV3:
			return s.handleStartup(client, br, int(msgLen))
		default:
			return fmt.Errorf("unsupported pg protocol code 0x%08x", code)
		}
	}
	return fmt.Errorf("client did not send StartupMessage after preamble")
}

func (s *Server) handleStartup(client net.Conn, br *bufio.Reader, msgLen int) error {
	body := make([]byte, msgLen-8)
	if _, err := io.ReadFull(br, body); err != nil {
		return fmt.Errorf("read startup body: %w", err)
	}
	params, err := ParseStartupBody(body)
	if err != nil {
		return err
	}

	dbname := LookupParam(params, "database")
	if dbname == "" {
		dbname = LookupParam(params, "user")
	}

	database := s.lookupDatabase(dbname)
	if database == nil {
		_ = WriteErrorResponse(client, "FATAL", "3D000", fmt.Sprintf("database %q not configured", dbname))
		s.logger.Info("unknown database", "dbname", dbname, "remote", client.RemoteAddr())
		return nil
	}

	rt, ok := database.Current()
	if !ok {
		_ = WriteErrorResponse(client, "FATAL", "57P03", fmt.Sprintf("database %q has no active target", dbname))
		return nil
	}

	// Build the upstream StartupMessage: target user, target database (or
	// pass-through), keep all other client-supplied parameters.
	upParams := []KV{{K: "user", V: rt.User}}
	if rt.Database != "" {
		upParams = append(upParams, KV{K: "database", V: rt.Database})
	} else if d := LookupParam(params, "database"); d != "" {
		upParams = append(upParams, KV{K: "database", V: d})
	}
	for _, kv := range params {
		if kv.K == "user" || kv.K == "database" {
			continue
		}
		upParams = append(upParams, kv)
	}
	upStartup := EncodeStartup(upParams)

	up, err := net.DialTimeout("tcp", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)), dialTimeout)
	if err != nil {
		_ = WriteErrorResponse(client, "FATAL", "08006", fmt.Sprintf("upstream dial failed: %v", err))
		s.logger.Error("upstream dial", "err", err, "addr", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)))
		return err
	}

	if rt.SSLMode == config.SSLModeRequire {
		tlsConn, err := NegotiateUpstreamTLS(up, rt.Host)
		if err != nil {
			_ = WriteErrorResponse(client, "FATAL", "08006", fmt.Sprintf("upstream TLS failed: %v", err))
			up.Close()
			s.logger.Error("upstream tls", "virtual_name", database.VirtualName(), "err", err)
			return err
		}
		up = tlsConn
	}

	if _, err := up.Write(upStartup); err != nil {
		_ = WriteErrorResponse(client, "FATAL", "08006", "upstream write failed")
		up.Close()
		return err
	}

	if err := AuthenticateUpstream(up, rt.User, rt.Password); err != nil {
		_ = WriteErrorResponse(client, "FATAL", "28P01", fmt.Sprintf("upstream auth failed: %v", err))
		up.Close()
		s.logger.Error("upstream auth", "virtual_name", database.VirtualName(), "err", err)
		return err
	}

	if err := WriteAuthenticationOk(client); err != nil {
		up.Close()
		return fmt.Errorf("write AuthenticationOk to client: %w", err)
	}

	// Flush any client bytes buffered past the StartupMessage to upstream.
	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		if _, err := up.Write(buf); err != nil {
			up.Close()
			return err
		}
		_, _ = br.Discard(n)
	}

	conn := &Conn{Client: client, Upstream: up, Resolved: rt}
	database.Register(conn)
	defer conn.Close()

	pipeBidi(client, up)
	return nil
}

// pipeBidi copies bytes in both directions until either side errors or EOFs.
func pipeBidi(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.Close()
		_ = b.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		_ = a.Close()
		_ = b.Close()
	}()
	wg.Wait()
}

// EncodeStartupForTarget is exposed for tests that want to assemble the
// upstream startup directly.
func EncodeStartupForTarget(clientParams []KV, rt ResolvedTarget) []byte {
	upParams := []KV{{K: "user", V: rt.User}}
	if rt.Database != "" {
		upParams = append(upParams, KV{K: "database", V: rt.Database})
	} else if d := LookupParam(clientParams, "database"); d != "" {
		upParams = append(upParams, KV{K: "database", V: d})
	}
	for _, kv := range clientParams {
		if kv.K == "user" || kv.K == "database" {
			continue
		}
		upParams = append(upParams, kv)
	}
	return EncodeStartup(upParams)
}

// SendStartupMessage is a convenience used by tests to send a complete
// StartupMessage of given params.
func SendStartupMessage(w io.Writer, params []KV) error {
	frame := EncodeStartup(params)
	_, err := w.Write(frame)
	return err
}

// SendCancelRequest writes a CancelRequest frame.
func SendCancelRequest(w io.Writer, pid, secret uint32) error {
	var buf [16]byte
	binary.BigEndian.PutUint32(buf[0:4], 16)
	binary.BigEndian.PutUint32(buf[4:8], CancelRequestCode)
	binary.BigEndian.PutUint32(buf[8:12], pid)
	binary.BigEndian.PutUint32(buf[12:16], secret)
	_, err := w.Write(buf[:])
	return err
}

// SendSSLRequest writes a single SSLRequest preamble.
func SendSSLRequest(w io.Writer) error {
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], SSLRequestCode)
	_, err := w.Write(buf[:])
	return err
}

func cloneVars(v map[string]string) map[string]string {
	if v == nil {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}
