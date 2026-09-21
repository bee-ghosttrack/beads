package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/oplog"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// commandOp is this invocation's in-flight op-log entry: set when the root
// PersistentPreRunE opens the store writable with oplog.dir configured, and
// finished by endCommandOplog on every exit path (main after ExecuteC, and
// CheckReadonly's os.Exit). Nil when the op log is off or the command reads.
var commandOp *oplog.Op

// beginCommandOplog writes the intent record for a write-classified command.
// It is called at the point the root hook has decided the store opens
// writable — the same classification (readOnlyCommands, --dry-run/--inspect,
// strict --readonly) every other write gate uses, so there is no second list
// of mutating verbs to drift. A failure to log warns and lets the command
// run: the op log is a detector, never a gate.
func beginCommandOplog(cmd *cobra.Command, args []string) {
	commandOp = nil
	dir := config.GetString("oplog.dir")
	if dir == "" {
		return
	}
	flags := map[string]string{}
	cmd.Flags().Visit(func(f *pflag.Flag) { flags[f.Name] = f.Value.String() })
	var ids []string
	for _, a := range args {
		if issueops.LooksLikeIssueID(a) {
			ids = append(ids, a)
		}
	}
	op, err := oplog.Begin(oplog.Options{Dir: dir, Sync: config.GetBool("oplog.sync")}, oplog.Intent{
		Actor: actor,
		Verb:  strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "),
		Args:  args,
		IDs:   ids,
		Flags: flags,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
		return
	}
	commandOp = op
}

// endCommandOplog writes the outcome record, once. Safe to call when no
// intent was logged.
func endCommandOplog(rc int) {
	if err := commandOp.End(rc); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
	}
}
