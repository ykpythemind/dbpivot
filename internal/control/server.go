package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/ykpythemind/db-pool-switch/internal/config"
	"github.com/ykpythemind/db-pool-switch/internal/proxy"
)

// Daemon is the interface the control server expects from the running
// proxy.Server: it can resolve pools, switch them, and report shutdown.
type Daemon interface {
	Pools() map[string]*proxy.Pool
	Addr() string
	IsClosed() bool
	Reload() (updated int, dropped int, warnings []string, err error)
}

// Server hosts the Unix-socket admin endpoint.
type Server struct {
	listener net.Listener
	daemon   Daemon
	logger   *slog.Logger
	cfg      *config.Config
}

// Listen creates and binds a Unix socket. Caller should also call os.Remove
// at shutdown.
func Listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// NewServer creates a control plane bound to the given listener.
func NewServer(ln net.Listener, daemon Daemon, cfg *config.Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{listener: ln, daemon: daemon, cfg: cfg, logger: logger}
}

// Serve runs the accept loop. It returns when the listener is closed.
func (s *Server) Serve() {
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("parse: %v", err)})
		return
	}

	if s.daemon.IsClosed() {
		s.writeResp(c, Response{OK: false, Error: "shutting down"})
		return
	}

	switch req.Cmd {
	case CmdSwitch:
		s.handleSwitch(c, &req)
	case CmdStatus:
		s.handleStatus(c, &req)
	case CmdList:
		s.handleList(c, &req)
	case CmdReload:
		s.handleReload(c, &req)
	default:
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("unknown command: %s", req.Cmd)})
	}
}

func (s *Server) writeResp(c net.Conn, resp Response) {
	out, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("marshal response", "err", err)
		return
	}
	out = append(out, '\n')
	_, _ = c.Write(out)
}

func (s *Server) handleSwitch(c net.Conn, req *Request) {
	if req.Pool == "" || req.Target == "" {
		s.writeResp(c, Response{OK: false, Error: "pool and target are required"})
		return
	}
	pool, ok := s.daemon.Pools()[req.Pool]
	if !ok {
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("unknown pool %q", req.Pool)})
		return
	}

	prev := pool.Current()
	prevRt, closedConns, missing, err := pool.Switch(req.Target, req.Variables)
	if err != nil {
		resp := Response{OK: false, Error: err.Error()}
		if len(missing) > 0 {
			resp.Missing = missing
		}
		s.writeResp(c, resp)
		return
	}
	// prevRt is the previous; the new state is reflected via pool.Current().
	cur := pool.Current()
	_ = prevRt
	s.writeResp(c, Response{
		OK:               true,
		Pool:             req.Pool,
		Previous:         prev.Name,
		PreviousDatabase: prev.Database,
		Current:          cur.Name,
		CurrentDatabase:  cur.Database,
		ClosedConns:      closedConns,
	})
}

func (s *Server) handleStatus(c net.Conn, req *Request) {
	resp := Response{OK: true, Port: s.cfg.Port}
	for name, pool := range s.daemon.Pools() {
		if req.Pool != "" && req.Pool != name {
			continue
		}
		cur := pool.Current()
		resp.Pools = append(resp.Pools, PoolStatus{
			Name:            name,
			Current:         cur.Name,
			CurrentDatabase: cur.Database,
			CurrentHost:     cur.Host,
			CurrentPort:     cur.Port,
			ActiveConns:     pool.ActiveConns(),
		})
	}
	if req.Pool != "" && len(resp.Pools) == 0 {
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("unknown pool %q", req.Pool)})
		return
	}
	s.writeResp(c, resp)
}

func (s *Server) handleList(c net.Conn, _ *Request) {
	resp := Response{OK: true, Port: s.cfg.Port}
	if len(s.cfg.ForwardTargets) > 0 {
		resp.ForwardTargets = make(map[string]ForwardTargetInfo, len(s.cfg.ForwardTargets))
		for name, ft := range s.cfg.ForwardTargets {
			resp.ForwardTargets[name] = ForwardTargetInfo{Host: ft.Host, Port: ft.Port}
		}
	}
	for _, p := range s.cfg.Pools {
		pool := s.daemon.Pools()[p.Name]
		pl := PoolList{Name: p.Name, Default: p.Default}
		if pool != nil {
			pl.Current = pool.Current().Name
		}
		for _, t := range p.Targets {
			vars := config.RequiredVars(t.Database)
			if vars == nil {
				vars = []string{}
			}
			pl.Targets = append(pl.Targets, TargetInfo{
				Name:              t.Name,
				Host:              t.Host,
				Port:              t.Port,
				ForwardTo:         t.ForwardTo,
				User:              t.User,
				DatabaseTemplate:  t.Database,
				RequiredVariables: vars,
			})
		}
		resp.ListPools = append(resp.ListPools, pl)
	}
	s.writeResp(c, resp)
}

func (s *Server) handleReload(c net.Conn, _ *Request) {
	updated, dropped, warnings, err := s.daemon.Reload()
	if err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error()})
		return
	}
	s.writeResp(c, Response{
		OK:           true,
		PoolsUpdated: updated,
		DroppedConns: dropped,
		Warnings:     warnings,
	})
}

// TryProbe attempts to connect to an existing daemon at socketPath; returns
// nil if a live daemon answers a `status` ping, or an error otherwise.
func TryProbe(socketPath string) (*Response, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"cmd":"status"}` + "\n")); err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// IsRefused reports whether err looks like ECONNREFUSED on a Unix socket.
func IsRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection refused")
}

// IsMissing reports whether err indicates the socket path does not exist.
func IsMissing(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file")
}
