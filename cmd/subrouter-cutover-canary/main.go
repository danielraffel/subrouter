package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/manaflow-ai/subrouter/internal/cutovercanary"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "subrouter cutover canary: proof failed")
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	signalContext, stopSignals := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalContext, 10*time.Minute)
	defer cancel()
	if len(args) > 0 {
		switch args[0] {
		case "peer-probe":
			if len(args) != 3 || args[1] != "--config" {
				return errors.New("usage")
			}
			result, err := cutovercanary.Probe(ctx, args[2])
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(result)
		case "witness":
			if len(args) != 5 || args[1] != "--challenge" || args[3] != "--witness" {
				return errors.New("usage")
			}
			return cutovercanary.CreateWitness(args[2], args[4], stdin)
		default:
			return errors.New("usage")
		}
	}
	leg := os.Getenv("SUBROUTER_CANARY_LEG_NAME")
	config := os.Getenv("SUBROUTER_CANARY_LEG_CONFIG_FILE")
	runID := os.Getenv("SUBROUTER_CANARY_RUN_ID")
	if leg == "" || config == "" || runID == "" {
		return errors.New("missing canary environment")
	}
	if err := cutovercanary.RunLeg(ctx, leg, config, runID); err != nil {
		return err
	}
	return cutovercanary.ServeLegResult(stdout, leg)
}
