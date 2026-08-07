package helpers

import (
	"strings"
	"testing"
	"time"
)

// runSplitWithTimeout runs SplitBody(body, maxRuneLen) and returns whether it finished within d.
func runSplitWithTimeout(body string, maxRuneLen int, d time.Duration) (done bool, chunks int) {
	ch := make(chan int, 1)
	go func() {
		res := SplitBody(body, maxRuneLen)
		ch <- len(res)
	}()
	select {
	case n := <-ch:
		return true, n
	case <-time.After(d):
		return false, 0
	}
}

// Diagnostic: raw terminal content containing unbalanced known-tag-like sequences (e.g. <b>, <td>)
// must not hang SplitBody. This reproduces the production /p bug where a 163KB pane capture with
// tag-like text spun SplitBody forever (findUnclosedTags treats <b> as a real tag and the prepend
// path fails to make progress). Escaping before split is the fix.
func TestSplitBody_RawTagLikeContentDoesNotHang(t *testing.T) {
	cases := map[string]string{
		"unbalanced_b_lines": strings.Repeat("<b>x\n", 40000),
		"leading_bare_tags":  strings.Repeat("<td><tr><li>", 20000),
		"mixed_terminal":     strings.Repeat("diff <b> a<c>d </b> code Vec<T> x>y </li>\n", 5000),
	}
	for name, body := range cases {
		body := body
		t.Run(name, func(t *testing.T) {
			done, n := runSplitWithTimeout(body, 3900, 15*time.Second)
			if !done {
				t.Fatalf("SplitBody HUNG on %s (len=%d bytes) — did not finish in 15s", name, len(body))
			}
			t.Logf("%s: finished, %d chunks", name, n)
		})
	}
}
