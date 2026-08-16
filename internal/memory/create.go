package memory

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrSpaceNotCreated is returned when `lore space create` reported success and no new
// space appeared. It is separate from a failure to run lore at all because the two are
// acted on differently: one is a broken install, the other is a lore that did something
// this code did not expect and must not be guessed at.
var ErrSpaceNotCreated = errors.New("memory: lore created no new space")

// CreateSpace makes a shared lore space and returns it.
//
// # Why this shells out
//
// lore's MCP server exposes five tools — search, get, put, share and spaces — and none
// of them creates anything. Space creation exists only on lore's own command line, as
// `lore space create <name>`. So until lore is a library, or grows a sixth tool, this is
// the only route: the same binary memory.Config already names, run as a one-shot
// subprocess against the same LORE_HOME.
//
// ponytail: subprocess, not a tool call. Replace the exec with a lore_space_create tool
// call, or with a library call, the moment either exists — Config.RunCLI is the seam and
// nothing above this function knows which one it got.
//
// # Why the id comes from a diff rather than from the output
//
// `lore space create` prints a line of human prose and this package already carries five
// parsers for lore's prose (see parse.go), each one a hostage to a wording change. There
// is a version-proof answer available for free: list the spaces, create, list again, and
// take the id that was not there before. It costs one extra tool call on an operation
// performed once per household member, and it cannot be broken by rewording.
//
// A creation that produces no new id, or more than one, is reported rather than guessed
// at. More than one means something else created a space at the same moment, and picking
// either would risk handing a member the wrong space — which is the one mistake in this
// whole product that silently publishes somebody's private memory.
func (c *Client) CreateSpace(ctx context.Context, name string) (Space, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return Space{}, fmt.Errorf("memory: a space needs a name: %w", ErrInvalidArgument)
	case strings.HasPrefix(name, "-"):
		// argv, not a shell, so there is no quoting to get wrong — but a leading
		// dash is still read by lore's own flag parser as a flag rather than as a
		// name, and this is a trust boundary: the name comes from a web form.
		return Space{}, fmt.Errorf("memory: a space name cannot start with %q, which lore would read as a flag: %w", "-", ErrInvalidArgument)
	case strings.ContainsAny(name, "\x00\n\r"):
		return Space{}, fmt.Errorf("memory: a space name cannot contain a newline or a null byte: %w", ErrInvalidArgument)
	}

	before, err := c.Spaces(ctx)
	if err != nil {
		return Space{}, err
	}
	known := make(map[string]bool, len(before))
	for _, s := range before {
		known[s.ID] = true
	}

	if out, err := c.runCLI(ctx, []string{"space", "create", name}); err != nil {
		return Space{}, fmt.Errorf("memory: creating lore space %q: %w%s", name, err, trailing(out))
	}

	after, err := c.Spaces(ctx)
	if err != nil {
		return Space{}, err
	}
	var created []Space
	for _, s := range after {
		if !known[s.ID] {
			created = append(created, s)
		}
	}
	switch len(created) {
	case 1:
		return created[0], nil
	case 0:
		return Space{}, fmt.Errorf("memory: lore reported creating %q and `lore spaces` does not list a new space: %w", name, ErrSpaceNotCreated)
	default:
		return Space{}, fmt.Errorf("memory: %d new spaces appeared while creating %q, so which one is it is not knowable from here; run `lore spaces` and configure the id by hand: %w",
			len(created), name, ErrSpaceNotCreated)
	}
}

// cliTimeout bounds one `lore` command-line invocation. It touches a local SQLite store
// and nothing else, so anything past this is wedged rather than slow.
const cliTimeout = 20 * time.Second

// runCLI runs lore's own command line with this client's environment, and returns its
// combined output for the error message.
func (c *Client) runCLI(ctx context.Context, args []string) ([]byte, error) {
	if c.cfg.RunCLI != nil {
		return c.cfg.RunCLI(ctx, args, c.cfg.childEnv(), c.cfg.Dir)
	}
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.cfg.Command, args...)
	cmd.Env = c.cfg.childEnv()
	cmd.Dir = c.cfg.Dir
	return cmd.CombinedOutput()
}

// trailing renders a subprocess's output for an error message, or nothing when it said
// nothing. lore's failures are one line, so this is never long.
func trailing(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return ": " + s
}
