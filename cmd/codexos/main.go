package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"codexos/internal/operator"
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func execute(arguments []string, input io.Reader, output, errorOutput io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := operator.NewCommand(
		operator.SupportsTUI(input, output, os.Getenv("TERM")),
		func(ctx context.Context, options operator.Options) error {
			return operator.RunWithIO(ctx, options, input, output)
		},
	)
	command.SetArgs(arguments)
	command.SetIn(input)
	command.SetOut(output)
	command.SetErr(errorOutput)
	if err := command.ExecuteContext(ctx); err != nil {
		var startupError *operator.StartupError
		if errors.As(err, &startupError) {
			fmt.Fprintln(output, "Error:", err)
			return 1
		}
		var executionError *operator.ExecutionError
		if errors.As(err, &executionError) {
			fmt.Fprintln(errorOutput, "Error:", err)
			return 1
		}
		fmt.Fprintln(errorOutput, "Error:", err)
		return 2
	}
	return 0
}
