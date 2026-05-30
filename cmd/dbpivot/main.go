package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/control"
	"github.com/ykpythemind/dbpivot/internal/proxy"
)

var (
	flagJSON     bool
	flagConfig   string
	flagLogLevel string
	flagVars     []string
	flagServeTgt string
)

func main() {
	root := &cobra.Command{
		Use:           "dbpivot",
		Short:         "A switchable local DB proxy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagConfig, "config", config.DefaultConfigPath, "path to config YAML")

	root.AddCommand(serveCmd())
	root.AddCommand(useCmd())
	root.AddCommand(statusCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func serveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe()
		},
	}
	c.Flags().StringVar(&flagLogLevel, "log-level", "info", "log level (debug|info|warn|error)")
	c.Flags().StringVar(&flagServeTgt, "target", "", "initial target to activate all databases with (required)")
	c.Flags().StringSliceVar(&flagVars, "var", nil, "variable in KEY=VAL form (may be repeated or comma-separated)")
	_ = c.MarkFlagRequired("target")
	return c
}

func runServe() error {
	logger := newLogger()
	cfg, err := config.Load(flagConfig, logger)
	if err != nil {
		return err
	}

	socketPath := cfg.ControlSocket
	if socketPath == "" {
		socketPath = config.DefaultControlSocket
	}

	if resp, perr := control.TryProbe(socketPath); perr == nil && resp.OK {
		body, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stderr, "another daemon is already running at %s: %s\n", socketPath, body)
		os.Exit(1)
	} else if perr != nil {
		if control.IsRefused(perr) {
			_ = os.Remove(socketPath)
		}
	}

	vars, err := parseVars(flagVars)
	if err != nil {
		return err
	}
	d, err := proxy.New(cfg, flagServeTgt, vars, logger)
	if err != nil {
		return err
	}

	// Minimal startup check: TCP-reachability of every declared upstream.
	// Warn-and-continue — the proxy dials upstreams lazily, so this is purely
	// an operator heads-up, never a hard gate.
	d.CheckReachability()

	ln, err := control.Listen(socketPath)
	if err != nil {
		return fmt.Errorf("bind control socket %s: %w", socketPath, err)
	}
	cs := control.NewServer(ln, d, cfg, logger)
	go cs.Serve()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Shutdown(ctx)
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()

	if err := d.Start(); err != nil {
		return err
	}
	return nil
}

func useCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "use <target>",
		Short: "Switch every database to <target>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vars, err := parseVars(flagVars)
			if err != nil {
				return err
			}
			resp, err := control.Call(config.LoadSocketPath(flagConfig), control.Request{
				Cmd: control.CmdUse, Target: args[0], Variables: vars,
			})
			if err != nil {
				return err
			}
			return renderUse(resp)
		},
	}
	c.Flags().StringSliceVar(&flagVars, "var", nil, "variable in KEY=VAL form (may be repeated or comma-separated)")
	c.Flags().BoolVar(&flagJSON, "json", false, "emit raw JSON response")
	return c
}

func statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the active target and per-database state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := control.Call(config.LoadSocketPath(flagConfig), control.Request{Cmd: control.CmdStatus})
			if err != nil {
				return err
			}
			return renderStatus(resp)
		},
	}
	c.Flags().BoolVar(&flagJSON, "json", false, "emit raw JSON response")
	return c
}

func parseVars(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string)
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			eq := strings.IndexByte(part, '=')
			if eq <= 0 {
				return nil, fmt.Errorf("invalid --var %q: must be KEY=VAL", part)
			}
			out[part[:eq]] = part[eq+1:]
		}
	}
	return out, nil
}

func renderUse(resp *control.Response) error {
	if flagJSON {
		emitJSON(resp)
		return exitErrIfNotOK(resp)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		if len(resp.Missing) > 0 {
			fmt.Fprintln(os.Stderr, "missing variables:", strings.Join(resp.Missing, ", "))
		}
		os.Exit(1)
	}
	fmt.Printf("switched to target %q:\n", resp.Target)
	for _, r := range resp.Switched {
		prev := r.Previous
		if prev == "" {
			prev = "-"
		}
		if r.Skipped {
			fmt.Printf("  %s: %s (db=%s) -> SKIPPED (no target %q; database is now inactive, closed %d connection(s))\n",
				r.VirtualName, prev, r.PreviousDatabase, resp.Target, r.ClosedConns)
			continue
		}
		fmt.Printf("  %s: %s (db=%s) -> %s (db=%s) (closed %d connection(s))\n",
			r.VirtualName, prev, r.PreviousDatabase,
			r.Current, r.CurrentDatabase, r.ClosedConns)
	}
	return nil
}

func renderStatus(resp *control.Response) error {
	if flagJSON {
		emitJSON(resp)
		return exitErrIfNotOK(resp)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	adapters := make([]string, 0, len(resp.Ports))
	for a := range resp.Ports {
		adapters = append(adapters, a)
	}
	sort.Strings(adapters)
	parts := make([]string, len(adapters))
	for i, a := range adapters {
		parts[i] = fmt.Sprintf("%s=127.0.0.1:%d", a, resp.Ports[a])
	}
	fmt.Printf("listening %s  current target: %s\n", strings.Join(parts, " "), resp.CurrentTarget)
	for _, d := range resp.Databases {
		if d.Inactive {
			fmt.Printf("  %s [%s] -> INACTIVE (no target %q declared; active=%d)\n",
				d.VirtualName, d.Adapter, resp.CurrentTarget, d.ActiveConns)
			continue
		}
		fmt.Printf("  %s [%s] -> %s (db=%s upstream=%s:%d active=%d)\n",
			d.VirtualName, d.Adapter, d.Current, d.CurrentDatabase, d.CurrentHost, d.CurrentPort, d.ActiveConns)
	}
	return nil
}

func emitJSON(resp *control.Response) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func exitErrIfNotOK(resp *control.Response) error {
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

func newLogger() *slog.Logger {
	var level slog.Level
	switch strings.ToLower(flagLogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
