// trainenv is an offline stdin/stdout worker for the Python training framework.
// It never opens network listeners or writes live application data.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"BattleCityReforge/internal/policymlp"
	"BattleCityReforge/internal/training"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "trainenv:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("trainenv", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	enemyModel := flags.String("enemy-model", "", "trusted local trained artifact directory for native enemy inference")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *enemyModel == "" {
		return training.Serve(in, out)
	}
	// Load once, before writing a hello or creating any simulation workers.
	// The artifact sampler is authoritative; there is no CLI override.
	model, err := policymlp.Load(*enemyModel)
	if err != nil {
		return fmt.Errorf("enemy model: %w", err)
	}
	if !model.Metadata().Trained {
		return fmt.Errorf("enemy model must have trained:true")
	}
	return training.ServeWithEnemyModel(in, out, model)
}
