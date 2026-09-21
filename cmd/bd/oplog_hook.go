package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/oplog"
)

// commandOp is this invocation's in-flight op-log entry: begun the first time
// the command passes CheckReadonly with oplog.dir configured, and finished by
// endCommandOplog on every exit path (main after ExecuteC, CheckReadonly's
// os.Exit, the second-signal force exit). Nil when the op log is off or the
// command never reached a write gate.
var commandOp *oplog.Op

// oplogCmd and oplogArgs are stashed at the very top of the root
// PersistentPreRunE, before any of its early returns, so a write that opens
// its own store (create --repo <remote URL>) is still logged when it reaches
// CheckReadonly.
var (
	oplogCmd  *cobra.Command
	oplogArgs []string
)

// oplogExcluded names commands that pass CheckReadonly but are not one write:
// serve is a long-lived process whose single "intent" would span every
// request it handles.
var oplogExcluded = map[string]bool{"serve": true}

// maxOplogIDLen caps each logged issue ID; real IDs are far shorter.
const maxOplogIDLen = 128

// oplogIDSuffix is what follows "<prefix>-" in a hash ID or a hierarchical
// child (abc12, abc12.1.3). A further hyphen means the argument is prose or a
// key name that merely starts with the prefix, so it is not logged.
var oplogIDSuffix = regexp.MustCompile(`^[A-Za-z0-9]+(\.[A-Za-z0-9]+)*$`)

// stashCommandOplog records the command for a later beginCommandOplog and
// clears any op left from a previous command in this process.
func stashCommandOplog(cmd *cobra.Command, args []string) {
	commandOp = nil
	oplogCmd = cmd
	oplogArgs = args
}

// beginCommandOplog writes the intent record, at most once per command. It is
// called from CheckReadonly once both of its gates pass: that function is the
// write chokepoint upstream already maintains (create, update, close and the
// rest call it before touching the store), so a read command that never calls
// it is never logged and there is no second list of mutating verbs to drift.
// A failure to log warns and lets the command run: the op log is a detector,
// never a gate.
func beginCommandOplog() {
	if commandOp != nil || oplogCmd == nil || oplogExcluded[oplogCmd.Name()] {
		return
	}
	dir := config.GetString("oplog.dir")
	if dir == "" {
		return
	}
	flags := map[string]string{}
	oplogCmd.Flags().Visit(func(f *pflag.Flag) { flags[f.Name] = f.Value.String() })
	op, err := oplog.Begin(oplog.Options{Dir: dir, Sync: config.GetBool("oplog.sync")}, oplog.Intent{
		Actor: actor,
		Verb:  strings.TrimPrefix(oplogCmd.CommandPath(), oplogCmd.Root().Name()+" "),
		Args:  oplogArgs,
		IDs:   oplogIssueIDs(oplogArgs, oplogIssuePrefix()),
		Flags: flags,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
		return
	}
	commandOp = op
}

// oplogIssuePrefix is the workspace's issue prefix: config.yaml first, as
// GetIssuePrefix orders it, then the store. Empty when neither is known, and
// then no IDs are logged.
func oplogIssuePrefix() string {
	if p := config.GetString("issue-prefix"); p != "" {
		return p
	}
	if store == nil {
		return ""
	}
	p, err := store.GetConfig(context.Background(), "issue_prefix")
	if err != nil {
		return ""
	}
	return p
}

// oplogIssueIDs keeps only the positional args shaped like this workspace's
// issue IDs. A hyphenated token is not enough: `kv set <key> <secret>` and
// `create <title>` carry hyphenated text that must never reach the log.
func oplogIssueIDs(args []string, prefix string) []string {
	if prefix == "" {
		return nil
	}
	var ids []string
	for _, a := range args {
		if len(a) > maxOplogIDLen {
			continue
		}
		rest, ok := strings.CutPrefix(a, prefix+"-")
		if ok && oplogIDSuffix.MatchString(rest) {
			ids = append(ids, a)
		}
	}
	return ids
}

// endCommandOplog writes the outcome record, once. Safe to call when no
// intent was logged.
func endCommandOplog(rc int) {
	if err := commandOp.End(rc); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
	}
}
