package main

import (
	"context"
	"errors"
	"fmt"
)

// Placeholders for the subcommands implemented in later milestones.
// Each is replaced by a real implementation with its own tests; a stub
// that pretends to work is worse than one that refuses.

var errNotImplemented = errors.New("not implemented yet")

func runDump(ctx context.Context, args []string) error   { return errNotImplemented }
func runVerify(ctx context.Context, args []string) error { return errNotImplemented }
func runFetch(ctx context.Context, args []string) error  { return errNotImplemented }
func runTune(ctx context.Context, args []string) error   { return errNotImplemented }

var _ = fmt.Sprint
