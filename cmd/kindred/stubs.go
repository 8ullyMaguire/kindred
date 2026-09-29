package main

import (
	"context"
	"errors"
)

// runTune inspects or sets the signal weights.
//
// The tune is a named weight vector over signals (SPEC §3.4), stored in
// the state database so it survives a restart and can be changed without a
// rebuild. It is the last subcommand still to land; the refusal is honest
// rather than a partial implementation that would write weights nothing
// reads.
func runTune(ctx context.Context, args []string) error {
	return errors.New("tune: not implemented yet; the default weights in engine.DefaultTune are in use")
}
