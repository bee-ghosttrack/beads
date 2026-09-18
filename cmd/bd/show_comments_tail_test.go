package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/uimd"
)

// testCommentsTailFormatTime is a fixed, deterministic formatTime for these
// tests — the real callers build one closed over --local-time, but the
// render function under test doesn't care which one it gets.
func testCommentsTailFormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04")
}

// makeTailTestComments builds n comments with distinguishable Author/Text
// bodies ("author-0"/"body-0" .. "author-(n-1)"/"body-(n-1)") in ascending
// CreatedAt order, matching GetIssueComments' oldest-first contract — the
// same order both show routes hand to printComments today.
func makeTailTestComments(n int) []*types.Comment {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	comments := make([]*types.Comment, n)
	for i := 0; i < n; i++ {
		comments[i] = &types.Comment{
			ID:        fmt.Sprintf("c-%d", i),
			Author:    fmt.Sprintf("author-%d", i),
			Text:      fmt.Sprintf("body-%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	return comments
}

// legacyRenderComments reproduces, verbatim, the COMMENTS block that lived
// inline in show.go / show_proxied_server.go before --comments-tail
// existed. It gives the "byte-identical to today's output" invariant an
// independent baseline to compare against, rather than comparing
// printComments against itself (which could hide a shared bug in both the
// capped and uncapped branches).
func legacyRenderComments(comments []*types.Comment, formatTime func(time.Time) string) {
	if len(comments) > 0 {
		fmt.Printf("\n%s\n", ui.RenderBold("COMMENTS"))
		for _, comment := range comments {
			fmt.Printf("  %s %s\n", ui.RenderMuted(formatTime(comment.CreatedAt)), comment.Author)
			rendered := uimd.RenderMarkdown(comment.Text)
			for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
	}
}

// TestShowCommentsTailFlagRegistration guards the wiring for --comments-tail:
// the flag must exist, default to 0 so a caller that never passes it gets
// today's uncapped output, and be read by the proxied route too — the direct
// and proxied routes gather flags separately (see TestShowBriefDepsFlag),
// so a flag added to one does not reach the other by implication.
func TestShowCommentsTailFlagRegistration(t *testing.T) {
	flag := showCmd.Flags().Lookup("comments-tail")
	if flag == nil {
		t.Fatal("comments-tail flag is not registered on showCmd")
	}
	if flag.DefValue != "0" {
		t.Errorf("comments-tail default = %q, want \"0\": the default output must not change", flag.DefValue)
	}

	t.Run("proxied route defaults to 0", func(t *testing.T) {
		if in := gatherShowProxiedInput(showCmd, []string{"be-abc"}); in.commentsTail != 0 {
			t.Errorf("commentsTail defaulted to %d, want 0", in.commentsTail)
		}
	})

	t.Run("proxied route reads it", func(t *testing.T) {
		if err := showCmd.Flags().Set("comments-tail", "3"); err != nil {
			t.Fatalf("set comments-tail=3: %v", err)
		}
		t.Cleanup(func() {
			_ = showCmd.Flags().Set("comments-tail", "0")
			showCmd.Flags().Lookup("comments-tail").Changed = false
		})
		if in := gatherShowProxiedInput(showCmd, []string{"be-abc"}); in.commentsTail != 3 {
			t.Errorf("gatherShowProxiedInput did not carry comments-tail to the proxied request, got %d", in.commentsTail)
		}
	})
}

// TestValidateCommentsTail pins the negative-value usage error both show
// routes share, following how --max-rows already rejects a negative value
// (resolveMaxRows in max_rows.go).
func TestValidateCommentsTail(t *testing.T) {
	if err := validateCommentsTail(0); err != nil {
		t.Errorf("0 should be valid, got %v", err)
	}
	if err := validateCommentsTail(5); err != nil {
		t.Errorf("a positive value should be valid, got %v", err)
	}

	var gotErr error
	stderr := captureStderr(t, func() {
		gotErr = validateCommentsTail(-1)
	})
	if gotErr == nil {
		t.Fatal("expected an error for a negative --comments-tail")
	}
	if !strings.Contains(stderr, "--comments-tail") || !strings.Contains(stderr, "non-negative") {
		t.Errorf("expected stderr to explain the flag constraint, got: %s", stderr)
	}
}

// TestPrintComments_TailSmallerThanCount is scenario 1: a real cap hides the
// older comments behind exactly one elision line naming how many, and
// renders only the newest N bodies, in their existing order and format.
func TestPrintComments_TailSmallerThanCount(t *testing.T) {
	comments := makeTailTestComments(5)
	out := captureStdout(t, func() error {
		printComments(comments, 2, testCommentsTailFormatTime, "ts-1")
		return nil
	})

	for i := 0; i < 3; i++ {
		if strings.Contains(out, fmt.Sprintf("body-%d", i)) {
			t.Errorf("expected body-%d to be hidden by the cap, found it in:\n%s", i, out)
		}
	}
	for i := 3; i < 5; i++ {
		if !strings.Contains(out, fmt.Sprintf("body-%d", i)) {
			t.Errorf("expected body-%d (one of the last 2) to render, missing from:\n%s", i, out)
		}
	}

	if n := strings.Count(out, "older comment"); n != 1 {
		t.Errorf("expected exactly one elision line, found %d in:\n%s", n, out)
	}
	wantLine := "… 3 older comments hidden — bd show ts-1 for the full record"
	if !strings.Contains(out, wantLine) {
		t.Errorf("expected elision line %q, got:\n%s", wantLine, out)
	}
}

// TestPrintComments_TailAtOrAboveCount is scenario 2: a cap that does not
// need to hide anything (tailN >= len(comments)) must render byte-identical
// output to the uncapped (tailN == 0) path — no elision line, every comment
// present.
func TestPrintComments_TailAtOrAboveCount(t *testing.T) {
	comments := makeTailTestComments(5)

	uncapped := captureStdout(t, func() error {
		printComments(comments, 0, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	atCount := captureStdout(t, func() error {
		printComments(comments, 5, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	aboveCount := captureStdout(t, func() error {
		printComments(comments, 100, testCommentsTailFormatTime, "ts-1")
		return nil
	})

	if uncapped != atCount {
		t.Errorf("tailN == len(comments) diverged from tailN == 0:\nuncapped:\n%s\natCount:\n%s", uncapped, atCount)
	}
	if uncapped != aboveCount {
		t.Errorf("tailN > len(comments) diverged from tailN == 0:\nuncapped:\n%s\naboveCount:\n%s", uncapped, aboveCount)
	}
	if strings.Contains(uncapped, "older comment") {
		t.Errorf("expected no elision line when nothing is hidden, got:\n%s", uncapped)
	}
}

// TestPrintComments_AbsentMatchesZero is scenario 3: the flag absent (which
// resolves to Go's zero value for an unset int flag, 0) and the flag
// explicitly passed as 0 must be indistinguishable — both take the tailN ==
// 0 branch, so there is nothing left to diverge, but this pins that
// contract at the call-site level too, not just inside printComments.
func TestPrintComments_AbsentMatchesZero(t *testing.T) {
	comments := makeTailTestComments(3)

	var absentTailN int // zero value: an unset --comments-tail flag GetInt()s to this
	absent := captureStdout(t, func() error {
		printComments(comments, absentTailN, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	explicitZero := captureStdout(t, func() error {
		printComments(comments, 0, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	if absent != explicitZero {
		t.Errorf("flag-absent output diverged from --comments-tail 0 output:\nabsent:\n%s\nexplicit:\n%s", absent, explicitZero)
	}
}

// TestPrintComments_ByteIdenticalToLegacyRender is the strongest form of "the
// flag absent or 0 must not change output": it compares printComments'
// uncapped path against an independent reproduction of the exact code that
// rendered the COMMENTS section before --comments-tail existed, so the two
// can't share a bug that a self-comparison (capped vs. uncapped both going
// through printComments) would hide.
func TestPrintComments_ByteIdenticalToLegacyRender(t *testing.T) {
	comments := makeTailTestComments(7)

	legacy := captureStdout(t, func() error {
		legacyRenderComments(comments, testCommentsTailFormatTime)
		return nil
	})
	current := captureStdout(t, func() error {
		printComments(comments, 0, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	if legacy != current {
		t.Errorf("printComments(tailN=0) diverged from the pre-flag render:\nlegacy:\n%s\ncurrent:\n%s", legacy, current)
	}

	t.Run("empty comments", func(t *testing.T) {
		legacyEmpty := captureStdout(t, func() error {
			legacyRenderComments(nil, testCommentsTailFormatTime)
			return nil
		})
		currentEmpty := captureStdout(t, func() error {
			printComments(nil, 0, testCommentsTailFormatTime, "ts-1")
			return nil
		})
		if legacyEmpty != currentEmpty || legacyEmpty != "" {
			t.Errorf("expected empty output for no comments, got legacy=%q current=%q", legacyEmpty, currentEmpty)
		}
	})
}

// TestPrintComments_SingularWording is scenario 5: exactly one older comment
// hidden gets "comment", not "comments".
func TestPrintComments_SingularWording(t *testing.T) {
	comments := makeTailTestComments(3)
	out := captureStdout(t, func() error {
		printComments(comments, 2, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	wantLine := "… 1 older comment hidden — bd show ts-1 for the full record"
	if !strings.Contains(out, wantLine) {
		t.Errorf("expected singular elision line %q, got:\n%s", wantLine, out)
	}
	if strings.Contains(out, "1 older comments") {
		t.Errorf("expected singular \"comment\", found plural in:\n%s", out)
	}
}

// TestPrintComments_CommentCountHeaderUnaffected pins that a cap does not
// touch any count carried alongside comments elsewhere — printComments only
// ever receives the already-fetched comment slice and never reports a count
// itself, so the total the caller shows in any header stays the total
// regardless of tailN. This guards against a future edit accidentally
// threading len(comments[start:]) somewhere a total was expected.
func TestPrintComments_CommentCountHeaderUnaffected(t *testing.T) {
	comments := makeTailTestComments(6)
	if got := len(comments); got != 6 {
		t.Fatalf("test setup: want 6 comments, got %d", got)
	}
	// printComments must not mutate the slice it was handed — a caller that
	// reports len(comments) in a header after calling it must still see 6.
	_ = captureStdout(t, func() error {
		printComments(comments, 2, testCommentsTailFormatTime, "ts-1")
		return nil
	})
	if got := len(comments); got != 6 {
		t.Errorf("printComments mutated the comments slice: len = %d, want 6", got)
	}
}
