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

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/proxy"
)

// Daemon is the interface the control server expects from the running
// proxy.Server.
type Daemon interface {
	Databases() map[string]*proxy.Database
	CurrentTarget() (string, map[string]string)
	SwitchAll(target string, vars map[string]string) ([]proxy.SwitchResult, error)
	ProbeActive() []proxy.ProbeResult
	Addrs() map[string]string
	IsClosed() bool
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
	case CmdUse:
		s.handleUse(c, &req)
	case CmdStatus:
		s.handleStatus(c, &req)
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

func (s *Server) handleUse(c net.Conn, req *Request) {
	if req.Target == "" {
		s.writeResp(c, Response{OK: false, Error: "target is required"})
		return
	}
	results, err := s.daemon.SwitchAll(req.Target, req.Variables)
	if err != nil {
		resp := Response{OK: false, Error: err.Error()}
		var planErr *proxy.SwitchPlanError
		if errors.As(err, &planErr) && len(planErr.Missing) > 0 {
			resp.Missing = planErr.Missing
		}
		s.writeResp(c, resp)
		return
	}
	wire := make([]SwitchResult, len(results))
	for i, r := range results {
		wire[i] = SwitchResult{
			VirtualName:      r.VirtualName,
			Previous:         r.Previous,
			PreviousDatabase: r.PreviousDatabase,
			Current:          r.Current,
			CurrentDatabase:  r.CurrentDatabase,
			ClosedConns:      r.ClosedConns,
			Skipped:          r.Skipped,
		}
	}
	// Health-check the newly-active targets (connect + auth + `select 1`).
	// Warn-and-continue: a failed probe does not undo the switch — the result
	// is reported to the operator both in the response and the daemon log.
	probes := s.daemon.ProbeActive()
	proxy.LogProbeResults(s.logger, probes)
	wireProbes := make([]ProbeResult, len(probes))
	for i, p := range probes {
		wireProbes[i] = ProbeResult{
			VirtualName: p.VirtualName,
			Target:      p.Target,
			Database:    p.Database,
			Addr:        p.Addr,
			OK:          p.OK,
			Err:         p.Err,
		}
	}

	s.writeResp(c, Response{
		OK:       true,
		Target:   req.Target,
		Switched: wire,
		Probes:   wireProbes,
	})
}

func (s *Server) handleStatus(c net.Conn, _ *Request) {
	target, _ := s.daemon.CurrentTarget()
	resp := Response{OK: true, Ports: s.cfg.ListenPorts, CurrentTarget: target}
	for name, d := range s.daemon.Databases() {
		ds := DatabaseStatus{VirtualName: name, Adapter: d.Adapter(), ActiveConns: d.ActiveConns()}
		if cur, ok := d.Current(); ok {
			ds.Current = cur.Name
			ds.CurrentDatabase = cur.Database
			ds.CurrentHost = cur.Host
			ds.CurrentPort = cur.Port
		} else {
			ds.Inactive = true
		}
		resp.Databases = append(resp.Databases, ds)
	}
	s.writeResp(c, resp)
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
