package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/oplog"
	"github.com/steveyegge/beads/internal/storage/sqltap"
)

// commandOp is this invocation's in-flight op-log entry: begun when the
// command sends its first mutating SQL statement with oplog.dir configured,
// and finished by endCommandOplog on every exit path (main after ExecuteC,
// CheckReadonly's os.Exit, the second-signal force exit). Nil when the op log
// is off or the command never wrote. Atomic because the signal goroutine ends
// it concurrently with main.
var commandOp atomic.Pointer[oplog.Op]

// oplogCmd and oplogArgs are stashed at the very top of the root
// PersistentPreRunE, before any of its early returns, so a write made while
// the hook is still opening the store is attributed to the command.
var (
	oplogCmd  *cobra.Command
	oplogArgs []string
)

// oplogExcluded names commands whose writes are not one operation: serve is a
// long-lived process whose single "intent" would span every request it
// handles.
var oplogExcluded = map[string]bool{"serve": true}

// stashCommandOplog records the command, clears any op left from a previous
// command in this process, and arms the SQL tap so the intent is written just
// before the command's first write leaves. Neither the CLI's write gate nor
// the store has one write chokepoint (previews pass the gate, some writes skip
// it, and transactions open at dozens of sites); every client write does pass
// through a tapped database/sql driver.
func stashCommandOplog(cmd *cobra.Command, args []string) {
	commandOp.Store(nil)
	oplogCmd = cmd
	oplogArgs = args
	if cmd == nil || oplogExcluded[cmd.Name()] {
		sqltap.Disarm()
		return
	}
	sqltap.Arm(beginCommandOplog)
}

// beginCommandOplog writes the intent record. It runs at most once per
// command, from inside the SQL tap, and must not itself send SQL. No issue
// IDs are logged: the command's arguments are not a reliable list of what it
// touched, and filtering them for ID-shaped tokens still leaked values. A
// failure to log warns and lets the write proceed: the op log is a detector,
// never a gate.
func beginCommandOplog() {
	if commandOp.Load() != nil || oplogCmd == nil {
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
		Flags: flags,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
		return
	}
	commandOp.Store(op)
}

// endCommandOplog writes the outcome record, once. Safe to call when no
// intent was logged, and from more than one goroutine.
func endCommandOplog(rc int) {
	if err := commandOp.Load().End(rc); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: op log: %v\n", err)
	}
}
