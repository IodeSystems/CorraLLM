package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/iodesystems/corrallm/internal/config"
)

// newSecretCmd manages the credential store (<home>/credentials) from a shell.
//
// The value is read from the terminal with echo off, or from stdin when piped,
// and never taken as an argument: an argument lands in shell history and in
// every `ps` listing for as long as the command runs.
//
// It writes the FILE. A running daemon read the store at start and keeps its
// copy, so a new value takes effect at the next `corrallm service restart`.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Set or list credential-store entries (values are never shown)"}
	var home string
	cmd.PersistentFlags().StringVar(&home, "home", defaultHome(), "corrallm home holding the credentials file")

	setCmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Store a secret, read from the terminal (no echo) or stdin",
		Long: "Stores NAME in <home>/credentials (0600). Config references it as ${NAME}.\n" +
			"An empty value removes the entry. A running daemon picks it up at its next restart.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.LoadCredentials(home); err != nil {
				return err
			}
			value, err := readSecret(cmd, args[0])
			if err != nil {
				return err
			}
			if err := config.SetSecret(args[0], value); err != nil {
				return err
			}
			if value == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "stored %s (%d chars); restart the daemon to use it: corrallm service restart\n", args[0], len(value))
			}
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the names in the credential store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.LoadCredentials(home); err != nil {
				return err
			}
			names := config.SecretNames()
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}

	cmd.AddCommand(setCmd, listCmd)
	return cmd
}

// readSecret reads one line: hidden from a terminal, plain from a pipe.
func readSecret(cmd *cobra.Command, name string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", name)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading %s from stdin: %w", name, err)
	}
	return strings.TrimSpace(line), nil
}
