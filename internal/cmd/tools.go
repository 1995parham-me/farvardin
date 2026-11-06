package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/auth"
)

// errLintFailed makes `farvardin lint` exit non-zero for CI.
var errLintFailed = errors.New("lint found errors")

func lintCommand() *cli.Command {
	//nolint:exhaustruct // command literal
	return &cli.Command{
		Name:      "lint",
		Usage:     "check .vcf files or directories of them; exits 1 when a card has errors",
		ArgsUsage: "<file or directory>...",
		Flags: []cli.Flag{
			//nolint:exhaustruct // flag literal
			&cli.BoolFlag{Name: "warnings", Aliases: []string{"w"}, Usage: "also print warnings"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			var files []string

			for _, arg := range cmd.Args().Slice() {
				fi, err := os.Stat(arg)
				if err != nil {
					return fmt.Errorf("lint: %w", err)
				}

				if !fi.IsDir() {
					files = append(files, arg)

					continue
				}

				found, _ := filepath.Glob(filepath.Join(arg, "*.vcf"))
				files = append(files, found...)
			}

			failed := 0

			for _, f := range files {
				data, err := os.ReadFile(f) //nolint:gosec // reading the files the user named is the point
				if err != nil {
					return fmt.Errorf("lint: %w", err)
				}

				var issues []contact.Issue

				if card, err := contact.Parse(data); err != nil {
					issues = []contact.Issue{{Severity: contact.Error, Message: err.Error()}}
				} else {
					issues = contact.Validate(card)
				}

				for _, i := range issues {
					if i.Severity == contact.Error || cmd.Bool("warnings") {
						fmt.Fprintf(os.Stdout, "%s: %s\n", f, i)
					}
				}

				if contact.HasErrors(issues) {
					failed++
				}
			}

			fmt.Fprintf(os.Stdout, "%d files, %d with errors\n", len(files), failed)

			if failed > 0 {
				return errLintFailed
			}

			return nil
		},
	}
}

func convertCommand() *cli.Command {
	//nolint:exhaustruct // command literal
	return &cli.Command{
		Name:      "convert",
		Usage:     "convert a vCard to vCard 3.0, vCard 4.0 or JSContact; reads stdin when no file is given",
		ArgsUsage: "[file]",
		Flags: []cli.Flag{
			//nolint:exhaustruct // flag literal
			&cli.StringFlag{Name: "to", Aliases: []string{"t"}, Value: "4.0", Usage: "3.0, 4.0 or jscontact"},
			//nolint:exhaustruct // flag literal
			&cli.BoolFlag{Name: "photos", Usage: "keep inline photos in JSContact output"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			var (
				data []byte
				err  error
			)

			if cmd.Args().Len() > 0 {
				data, err = os.ReadFile(cmd.Args().First())
			} else {
				data, err = io.ReadAll(os.Stdin)
			}

			if err != nil {
				return fmt.Errorf("convert: %w", err)
			}

			cards, err := contact.ParseAll(data)
			if err != nil {
				return fmt.Errorf("convert: %w", err)
			}

			to := strings.ToLower(cmd.String("to"))

			for _, c := range cards {
				if to == "jscontact" || to == "json" {
					out, err := json.MarshalIndent(contact.ToJSContact(c, cmd.Bool("photos")), "", "  ")
					if err != nil {
						return fmt.Errorf("convert: %w", err)
					}

					fmt.Fprintln(os.Stdout, string(out))

					continue
				}

				out, err := contact.Convert(c, to)
				if err != nil {
					return fmt.Errorf("convert: %w", err)
				}

				_, _ = os.Stdout.Write(out.Encode())
			}

			return nil
		},
	}
}

func hashPasswordCommand() *cli.Command {
	//nolint:exhaustruct // command literal
	return &cli.Command{
		Name:  "hash-password",
		Usage: "print a bcrypt hash for [auth.users]; reads the password from the terminal or stdin",
		Action: func(_ context.Context, _ *cli.Command) error {
			var (
				pw  []byte
				err error
			)

			if term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Fprint(os.Stderr, "password: ")
				pw, err = term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
			} else {
				pw, err = io.ReadAll(os.Stdin)
			}

			if err != nil {
				return fmt.Errorf("reading password: %w", err)
			}

			h, err := auth.Hash(strings.TrimRight(string(pw), "\r\n"))
			if err != nil {
				return fmt.Errorf("hashing: %w", err)
			}

			fmt.Fprintln(os.Stdout, h)

			return nil
		},
	}
}
