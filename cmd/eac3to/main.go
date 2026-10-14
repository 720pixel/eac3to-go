package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"eac3to/internal/app"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.RunWithInput(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, app.ErrInteractiveCancelled) {
			fmt.Fprintln(os.Stderr, "Cancelled.")
			return
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "Cancelled.")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "eac3to:", err)
		os.Exit(1)
	}
}
