package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/open-rails/helpers/devproxy"
)

type companions [][]string

func (c *companions) String() string { return fmt.Sprint([][]string(*c)) }
func (c *companions) Set(value string) error {
	var args []string
	if err := json.Unmarshal([]byte(value), &args); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("companion requires a JSON argument array")
	}
	*c = append(*c, args)
	return nil
}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "devproxy:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: devproxy {serve|deploy|status} --control /absolute/socket")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	control := flags.String("control", "", "private Unix control socket")
	switch os.Args[1] {
	case "serve":
		cfg := devproxy.Config{}
		flags.StringVar(&cfg.Listen, "listen", "127.0.0.1:2052", "stable public HTTP listener")
		flags.DurationVar(&cfg.ReadyTimeout, "ready-timeout", 0, "candidate readiness deadline (default 60s)")
		flags.DurationVar(&cfg.DrainTimeout, "drain-timeout", 0, "old requests/WebSocket drain deadline (default 30s)")
		flags.DurationVar(&cfg.StopTimeout, "stop-timeout", 0, "graceful process-group shutdown deadline (default 10s)")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		cfg.Control = *control
		return devproxy.Run(ctx, cfg)
	case "deploy":
		var other companions
		flags.Var(&other, "companion", "repeatable JSON argument array for another process using the candidate executable")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		args := flags.Args()
		if len(args) == 0 {
			return errors.New("deploy requires -- executable [arguments]")
		}
		bin, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		result, err := devproxy.Deploy(ctx, *control, devproxy.Deployment{Executable: bin, Args: args[1:], Companions: other, Directory: cwd, Environment: os.Environ()})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "status":
		expectPID := flags.Int("expect-pid", 0, "require this controller process ID")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		result, err := devproxy.Inspect(ctx, *control)
		if err != nil {
			return err
		}
		if *expectPID > 0 && result.PID != *expectPID {
			return fmt.Errorf("control socket belongs to process %d, expected %d", result.PID, *expectPID)
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	default:
		return errors.New("usage: devproxy {serve|deploy|status} --control /absolute/socket")
	}
}
