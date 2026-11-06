// Package cmd builds the farvardin command tree.
package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/carlmjohnson/versioninfo"
	"github.com/urfave/cli/v3"
)

// Execute runs the command line and exits on error.
func Execute() {
	//nolint:exhaustruct // a CLI root: only the fields in use are set
	root := &cli.Command{
		Name:    "farvardin",
		Usage:   "CardDAV and CalDAV server backed by git",
		Version: versioninfo.Short(),
		Flags: []cli.Flag{
			//nolint:exhaustruct // flag literal
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to a TOML config file", Sources: cli.EnvVars("FARVARDIN_CONFIG")},
		},
		Commands: []*cli.Command{serveCommand(), lintCommand(), convertCommand(), hashPasswordCommand(), mcpTokenCommand()},
	}

	if err := root.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "farvardin:", err)
		os.Exit(1)
	}
}
