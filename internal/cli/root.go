// Package cli dispatches subcommands. Each command lives in its own file and
// registers itself via Register in an init func; root.go owns dispatch, help
// and exit codes.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/muratgozel/seasoning/internal/selfupdate"
	"github.com/muratgozel/seasoning/internal/version"
)

// Exit codes. ExitUsage is for bad flags/args; ExitError for runtime failures.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is what a command receives. Commands write only to Stdout/Stderr
// (never os.Stdout) so tests can capture output.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads the environment; tests replace it.
	Getenv func(string) string
	// Dir is the working directory commands act on.
	Dir string
}

// Command is one subcommand. Run returns an error for runtime failures and a
// *UsageError (via Usagef) for bad invocation.
type Command struct {
	Name    string
	Summary string // one line, shown in `seasoning help`
	Usage   string // synopsis, e.g. "join INVITE [--name NAME]"
	Hidden  bool
	Run     func(env Env) error
}

// ExitStatus ends the command with Code and prints nothing: the command
// already reported what it had to (a hook's exit 2 that wakes Claude).
type ExitStatus struct{ Code int }

func (e *ExitStatus) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Exit builds an ExitStatus.
func Exit(code int) error { return &ExitStatus{Code: code} }

// UsageError marks an invocation error; Main maps it to ExitUsage.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usagef builds a UsageError.
func Usagef(format string, a ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, a...)}
}

var registry = map[string]*Command{}

// hint prints the one-time "updated" note or the "update available" note;
// a var for tests.
var hint = selfupdate.Hint

// Register adds a command; called from init funcs. Panics on duplicates.
func Register(c *Command) {
	if c == nil || c.Name == "" || c.Run == nil {
		panic("cli: Register requires Name and Run")
	}
	if _, dup := registry[c.Name]; dup {
		panic("cli: duplicate command " + c.Name)
	}
	registry[c.Name] = c
}

// NewFlagSet returns a FlagSet that reports to env.Stderr and returns errors.
func NewFlagSet(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// parse parses flags and rejects positional arguments beyond want.
func parse(fs *flag.FlagSet, args []string, want int) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, &UsageError{}
		}
		return nil, &UsageError{Msg: err.Error()}
	}
	rest := fs.Args()
	// Accept flags after positional arguments too ("join INVITE --name x").
	if len(rest) > 0 && want > 0 && len(rest) > want {
		if err := fs.Parse(rest[want:]); err != nil {
			return nil, &UsageError{Msg: err.Error()}
		}
		rest = append(rest[:want:want], fs.Args()...)
	}
	if len(rest) != want {
		return nil, Usagef("expected %d argument(s), got %d: %s", want, len(rest), strings.Join(rest, " "))
	}
	return rest, nil
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "seasoning: cannot read the working directory: %v\n", err)
		return ExitError
	}
	return Run(Env{Args: args, Stdin: stdin, Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, Dir: dir})
}

// Run dispatches env.Args[0] with the given environment.
func Run(env Env) int {
	args := env.Args
	if len(args) == 0 {
		printHelp(env.Stdout)
		return ExitUsage
	}
	name := args[0]
	switch name {
	case "help", "-h", "--help":
		if len(args) > 1 {
			if c, ok := registry[args[1]]; ok {
				fmt.Fprintf(env.Stdout, "usage: seasoning %s\n\n%s\n", c.Usage, c.Summary)
				return ExitOK
			}
		}
		printHelp(env.Stdout)
		return ExitOK
	case "version", "-v", "--version":
		fmt.Fprintf(env.Stdout, "seasoning %s\n", version.Version)
		return ExitOK
	}
	c, ok := registry[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "seasoning: unknown command %q (run `seasoning help`)\n", name)
		return ExitUsage
	}
	env.Args = args[1:]
	if !c.Hidden && c.Name != "update" && c.Name != "serve" {
		hint(env.Stderr)
	}
	err := c.Run(env)
	var ue *UsageError
	var es *ExitStatus
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &es):
		return es.Code
	case errors.As(err, &ue):
		if ue.Msg != "" {
			fmt.Fprintf(env.Stderr, "seasoning %s: %s\n", name, ue.Msg)
		}
		fmt.Fprintf(env.Stderr, "usage: seasoning %s\n", c.Usage)
		return ExitUsage
	default:
		fmt.Fprintf(env.Stderr, "seasoning %s: %v\n", name, err)
		return ExitError
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, "seasoning %s: a relay for coding agents working on the same project\n\nCommands:\n", version.Version)
	var names []string
	for n, c := range registry {
		if !c.Hidden {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-10s %s\n", n, registry[n].Summary)
	}
	fmt.Fprintf(w, "\nRun `seasoning help COMMAND` for a command's usage.\n")
}
