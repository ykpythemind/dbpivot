package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/control"
	"github.com/ykpythemind/dbpivot/internal/proxy"
)

var (
	flagSocket   string
	flagJSON     bool
	flagConfig   string
	flagLogLevel string
	flagVars     []string
)

func main() {
	root := &cobra.Command{
		Use:           "dbpivot",
		Short:         "A switchable local DB proxy",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagSocket, "socket", config.DefaultControlSocket, "control socket path")

	root.AddCommand(serveCmd())
	root.AddCommand(switchCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(listCmd())
	root.AddCommand(reloadCmd())

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
	c.Flags().StringVar(&flagConfig, "config", "", "path to config YAML (required)")
	c.Flags().StringVar(&flagLogLevel, "log-level", "info", "log level (debug|info|warn|error)")
	_ = c.MarkFlagRequired("config")
	return c
}

func runServe() error {
	logger := newLogger()
	cfg, err := config.Load(flagConfig, logger)
	if err != nil {
		return err
	}

	socketPath := flagSocket
	if cfg.ControlSocket != "" && socketPath == config.DefaultControlSocket {
		socketPath = cfg.ControlSocket
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

	d, err := proxy.New(cfg, flagConfig, logger)
	if err != nil {
		return err
	}

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

func switchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "switch <pool> <target>",
		Short: "Switch a pool to a target",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vars, err := parseVars(flagVars)
			if err != nil {
				return err
			}
			resp, err := control.Call(flagSocket, control.Request{
				Cmd: control.CmdSwitch, Pool: args[0], Target: args[1], Variables: vars,
			})
			if err != nil {
				return err
			}
			return renderSwitch(resp)
		},
	}
	c.Flags().StringSliceVar(&flagVars, "var", nil, "variable in KEY=VAL form (may be repeated or comma-separated)")
	c.Flags().BoolVar(&flagJSON, "json", false, "emit raw JSON response")
	return c
}

func statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status [pool]",
		Short: "Show current target(s)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Request{Cmd: control.CmdStatus}
			if len(args) == 1 {
				req.Pool = args[0]
			}
			resp, err := control.Call(flagSocket, req)
			if err != nil {
				return err
			}
			return renderStatus(resp)
		},
	}
	c.Flags().BoolVar(&flagJSON, "json", false, "emit raw JSON response")
	return c
}

func listCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List pools and their configured targets",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := control.Call(flagSocket, control.Request{Cmd: control.CmdList})
			if err != nil {
				return err
			}
			return renderList(resp)
		},
	}
	c.Flags().BoolVar(&flagJSON, "json", false, "emit raw JSON response")
	return c
}

func reloadCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reload",
		Short: "Re-read the config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := control.Call(flagSocket, control.Request{Cmd: control.CmdReload})
			if err != nil {
				return err
			}
			return renderReload(resp)
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

func renderSwitch(resp *control.Response) error {
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
	fmt.Printf("%s: %s (db=%s) -> %s (db=%s) (closed %d connection(s))\n",
		resp.Pool, resp.Previous, resp.PreviousDatabase,
		resp.Current, resp.CurrentDatabase, resp.ClosedConns)
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
	fmt.Printf("listening on 127.0.0.1:%d\n", resp.Port)
	for _, p := range resp.Pools {
		fmt.Printf("  %s -> %s (db=%s upstream=%s:%d active=%d)\n",
			p.Name, p.Current, p.CurrentDatabase, p.CurrentHost, p.CurrentPort, p.ActiveConns)
	}
	return nil
}

func renderList(resp *control.Response) error {
	if flagJSON {
		emitJSON(resp)
		return exitErrIfNotOK(resp)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	fmt.Printf("port: %d\n", resp.Port)
	if len(resp.ForwardTargets) > 0 {
		fmt.Println("forward_targets:")
		for name, ft := range resp.ForwardTargets {
			fmt.Printf("  %s -> %s:%d\n", name, ft.Host, ft.Port)
		}
	}
	for _, pl := range resp.ListPools {
		fmt.Printf("pool %s (default=%s, current=%s):\n", pl.Name, pl.Default, pl.Current)
		for _, t := range pl.Targets {
			endpoint := ""
			if t.ForwardTo != "" {
				endpoint = "forward_to=" + t.ForwardTo
			} else {
				endpoint = fmt.Sprintf("%s:%d", t.Host, t.Port)
			}
			vars := ""
			if len(t.RequiredVariables) > 0 {
				vars = " vars=" + strings.Join(t.RequiredVariables, ",")
			}
			fmt.Printf("  %-12s %s user=%s db=%q%s\n", t.Name, endpoint, t.User, t.DatabaseTemplate, vars)
		}
	}
	return nil
}

func renderReload(resp *control.Response) error {
	if flagJSON {
		emitJSON(resp)
		return exitErrIfNotOK(resp)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	fmt.Printf("reloaded: %d pool(s) updated, %d connection(s) dropped\n", resp.PoolsUpdated, resp.DroppedConns)
	for _, w := range resp.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
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
