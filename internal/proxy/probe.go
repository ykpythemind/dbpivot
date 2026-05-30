package proxy

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// probeTimeout bounds the auth + query phase of a single probe (the initial
// dial is separately bounded by dialTimeout). It is generous enough for a slow
// upstream handshake but keeps serve/use from hanging on a black-hole endpoint.
const probeTimeout = 5 * time.Second

// comQuery is the MySQL COM_QUERY command byte.
const comQuery = 0x03

// ProbeResult is the outcome of one upstream health probe: a connect + auth +
// `select 1` round-trip against a single database's currently-active target.
type ProbeResult struct {
	VirtualName string
	Target      string
	Database    string
	Addr        string
	OK          bool
	Err         string // populated when OK is false
}

// ProbeActive probes every database's currently-active target with a
// connect + auth + `select 1` round-trip and returns one result per active
// database. Databases with no active target (INACTIVE) are skipped. It never
// mutates routing state and never fails the caller; probes run concurrently and
// results are returned in unspecified order.
func (s *Server) ProbeActive() []ProbeResult {
	type job struct {
		name    string
		adapter string
		rt      ResolvedTarget
	}
	var jobs []job
	for name, d := range s.Databases() {
		rt, ok := d.Current()
		if !ok {
			continue
		}
		jobs = append(jobs, job{name: name, adapter: d.Adapter(), rt: rt})
	}

	results := make([]ProbeResult, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			r := ProbeResult{
				VirtualName: j.name,
				Target:      j.rt.Name,
				Database:    j.rt.Database,
				Addr:        net.JoinHostPort(j.rt.Host, strconv.Itoa(j.rt.Port)),
			}
			if err := probeTarget(j.adapter, j.rt); err != nil {
				r.Err = err.Error()
			} else {
				r.OK = true
			}
			results[i] = r
		}(i, j)
	}
	wg.Wait()
	return results
}

// LogProbeResults emits one log line per probe result: INFO for a healthy
// upstream, WARN (with the error) for a failed one.
func LogProbeResults(logger *slog.Logger, results []ProbeResult) {
	for _, r := range results {
		if r.OK {
			logger.Info("upstream probe ok", "virtual_name", r.VirtualName, "target", r.Target, "addr", r.Addr)
		} else {
			logger.Warn("upstream probe failed", "virtual_name", r.VirtualName, "target", r.Target, "addr", r.Addr, "err", r.Err)
		}
	}
}

// probeTarget runs the adapter-appropriate connect + auth + `select 1` probe.
func probeTarget(adapter string, rt ResolvedTarget) error {
	switch adapter {
	case config.AdapterMySQL:
		return probeMySQL(rt)
	default:
		return probePostgres(rt)
	}
}

// probePostgres dials the upstream, completes the PG startup + SCRAM handshake
// exactly as handleStartup does, then issues a single `select 1` and waits for
// ReadyForQuery, surfacing any ErrorResponse as the probe error.
func probePostgres(rt ResolvedTarget) error {
	addr := net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))

	if rt.SSLMode == config.SSLModeRequire {
		tlsConn, err := NegotiateUpstreamTLS(conn, rt.Host)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		conn = tlsConn
	}

	upParams := []KV{{K: "user", V: rt.User}}
	if rt.Database != "" {
		upParams = append(upParams, KV{K: "database", V: rt.Database})
	}
	if _, err := conn.Write(EncodeStartup(upParams)); err != nil {
		return fmt.Errorf("write startup: %w", err)
	}
	if err := AuthenticateUpstream(conn, rt.User, rt.Password); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	// AuthenticateUpstream stops right after AuthenticationOk; drain the
	// ParameterStatus/BackendKeyData burst until the server is ready.
	if err := pgWaitReady(conn); err != nil {
		return fmt.Errorf("post-auth: %w", err)
	}
	if err := WriteMessage(conn, 'Q', append([]byte("select 1"), 0)); err != nil {
		return fmt.Errorf("write query: %w", err)
	}
	if err := pgWaitReady(conn); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	return nil
}

// pgWaitReady reads PG messages until ReadyForQuery ('Z'), returning the
// decoded text of the first ErrorResponse ('E') it encounters as an error.
func pgWaitReady(conn net.Conn) error {
	for {
		typ, body, err := ReadMessage(conn)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		switch typ {
		case 'E':
			return fmt.Errorf("%s", ParseErrorResponse(body))
		case 'Z':
			return nil
		}
	}
}

// probeMySQL dials the upstream, completes the MySQL login exactly as
// dispatchMySQL does, then issues a single `select 1` COM_QUERY and inspects the
// first response packet, surfacing an ERR packet as the probe error.
func probeMySQL(rt ResolvedTarget) error {
	addr := net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))

	var tlsConfig *tls.Config
	if rt.SSLMode == config.SSLModeRequire {
		tlsConfig = &tls.Config{
			ServerName:         rt.Host,
			InsecureSkipVerify: true, // sslmode=require: encrypt, don't verify
		}
	}
	upConn, _, _, err := AuthenticateUpstreamMySQL(conn, rt.User, rt.Password, rt.Database, false, tlsConfig)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	conn = upConn

	payload := append([]byte{comQuery}, []byte("select 1")...)
	if _, err := WritePacket(conn, 0, payload); err != nil {
		return fmt.Errorf("write query: %w", err)
	}
	_, resp, err := ReadPacket(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if IsErrPacket(resp) {
		return fmt.Errorf("%s", ParseERRPacket(resp))
	}
	return nil
}
