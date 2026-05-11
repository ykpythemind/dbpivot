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
	"time"

	"github.com/ykpythemind/db-pool-switch/internal/config"
)

const dialTimeout = 5 * time.Second

// Server hosts the single TCP listener and the per-pool routing state.
type Server struct {
	addr    string
	cfgPath string
	logger  *slog.Logger

	listener net.Listener

	mu     sync.RWMutex
	pools  map[string]*Pool
	cfg    *config.Config
	closed bool
	wg     sync.WaitGroup
}

// New constructs a Server from validated config. It does not open any
// listeners yet — call Start.
func New(cfg *config.Config, cfgPath string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pools := make(map[string]*Pool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		pool, err := NewPool(p, cfg.ForwardTargets)
		if err != nil {
			return nil, err
		}
		pools[p.Name] = pool
	}
	return &Server{
		addr:    net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)),
		cfgPath: cfgPath,
		pools:   pools,
		cfg:     cfg,
		logger:  logger,
	}, nil
}

// Pools returns a snapshot of the live pools keyed by name.
func (s *Server) Pools() map[string]*Pool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*Pool, len(s.pools))
	for k, v := range s.pools {
		out[k] = v
	}
	return out
}

func (s *Server) lookupPool(name string) *Pool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pools[name]
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
	s.logger.Info("listening", "addr", ln.Addr())
	for name, p := range s.Pools() {
		c := p.Current()
		s.logger.Info("pool ready", "pool", name, "current", c.Name, "database", c.Database, "upstream", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
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
	pools := s.pools
	ln := s.listener
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, p := range pools {
		p.DropAll()
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
	if err := s.dispatch(client); err != nil {
		if !errors.Is(err, io.EOF) {
			s.logger.Warn("conn closed", "remote", client.RemoteAddr(), "err", err)
		}
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

	pool := s.lookupPool(dbname)
	if pool == nil {
		_ = WriteErrorResponse(client, "FATAL", "3D000", fmt.Sprintf("pool %q not configured", dbname))
		s.logger.Info("unknown pool", "dbname", dbname, "remote", client.RemoteAddr())
		return nil
	}

	rt := pool.Current()

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

	if _, err := up.Write(upStartup); err != nil {
		_ = WriteErrorResponse(client, "FATAL", "08006", "upstream write failed")
		up.Close()
		return err
	}

	if err := AuthenticateUpstream(up, rt.User, rt.Password); err != nil {
		_ = WriteErrorResponse(client, "FATAL", "28P01", fmt.Sprintf("upstream auth failed: %v", err))
		up.Close()
		s.logger.Error("upstream auth", "pool", pool.Name(), "err", err)
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
	pool.Register(conn)
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

// Reload re-reads the config file and swaps in new pool state. All active
// connections in the affected pools are dropped. Changes to `port` are
// reported as warnings — port hot-swap is not supported.
func (s *Server) Reload() (updated int, dropped int, warnings []string, err error) {
	if s.cfgPath == "" {
		return 0, 0, nil, fmt.Errorf("no config path recorded; reload unavailable")
	}
	newCfg, err := config.Load(s.cfgPath, s.logger)
	if err != nil {
		return 0, 0, nil, err
	}

	s.mu.Lock()
	oldCfg := s.cfg
	oldPools := s.pools
	s.mu.Unlock()

	if oldCfg != nil && newCfg.Port != oldCfg.Port {
		warnings = append(warnings, fmt.Sprintf("port change requires restart (running=%d, config=%d)", oldCfg.Port, newCfg.Port))
	}

	newPools := make(map[string]*Pool, len(newCfg.Pools))
	for _, p := range newCfg.Pools {
		pool, perr := NewPool(p, newCfg.ForwardTargets)
		if perr != nil {
			return 0, 0, nil, perr
		}
		newPools[p.Name] = pool
	}

	for name := range oldPools {
		if _, ok := newPools[name]; !ok {
			warnings = append(warnings, fmt.Sprintf("pool %q removed (restart not required for routing changes, but new dbname=%q will be rejected after reload)", name, name))
		}
	}

	for _, p := range oldPools {
		dropped += p.DropAll()
	}

	s.mu.Lock()
	s.pools = newPools
	s.cfg = newCfg
	s.mu.Unlock()
	return len(newPools), dropped, warnings, nil
}

// Config returns the currently loaded config (for control plane listings).
func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SendSSLRequest writes a single SSLRequest preamble.
func SendSSLRequest(w io.Writer) error {
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[0:4], 8)
	binary.BigEndian.PutUint32(buf[4:8], SSLRequestCode)
	_, err := w.Write(buf[:])
	return err
}
