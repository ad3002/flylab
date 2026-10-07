// Package cli implements the `flylab user ...` subcommands (contract section 6).
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ad3002/flylab/internal/auth"
	"github.com/ad3002/flylab/internal/storage"
)

const userUsage = "usage: flylab user create --username U --password P [--display-name N] | flylab user passwd --username U --password P"

// RunUser executes `flylab user <args>` against store. It prints one JSON line to stdout on
// success and one JSON line to stderr on failure, and returns the process exit code.
func RunUser(args []string, store *storage.Store, stdout, stderr io.Writer) int {
	result, err := runUser(args, store)
	if err != nil {
		writeLine(stderr, map[string]interface{}{"ok": false, "error": err.Error()})
		return 1
	}
	writeLine(stdout, result)
	return 0
}

func runUser(args []string, store *storage.Store) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, errors.New(userUsage)
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("flylab user "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "account name")
	password := fs.String("password", "", "password (8-128 characters)")
	var displayName *string
	if sub == "create" {
		displayName = fs.String("display-name", "", "display name (defaults to the username)")
	}

	switch sub {
	case "create", "passwd":
	default:
		return nil, fmt.Errorf("unknown subcommand %q; %s", sub, userUsage)
	}
	if err := fs.Parse(rest); err != nil {
		return nil, fmt.Errorf("%v; %s", err, userUsage)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments %v; %s", fs.Args(), userUsage)
	}

	name, err := auth.NormalizeUsername(*username)
	if err != nil {
		return nil, err
	}
	if err := auth.ValidatePassword(*password); err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(*password)
	if err != nil {
		return nil, err
	}

	switch sub {
	case "create":
		display, err := auth.NormalizeDisplayName(*displayName, name)
		if err != nil {
			return nil, err
		}
		user, err := store.CreateUser(name, display, hash)
		if err != nil {
			if errors.Is(err, storage.ErrUsernameTaken) {
				return nil, fmt.Errorf("username %q is already taken", name)
			}
			return nil, err
		}
		return map[string]interface{}{"ok": true, "action": "created", "user": user}, nil
	default: // passwd
		user, revoked, err := store.UpdatePassword(name, hash)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, fmt.Errorf("user %q does not exist", name)
			}
			return nil, err
		}
		return map[string]interface{}{"ok": true, "action": "password_changed", "user": user, "sessions_revoked": revoked}, nil
	}
}

func writeLine(w io.Writer, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(w, "{\"ok\":false,\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintln(w, string(b))
}
