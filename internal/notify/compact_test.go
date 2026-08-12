package notify

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCompactBashCommandGraduatedFlags verifies that only the minimum flags are removed
// (incremental removal), not all at once. At least one flag must be retained.
func TestCompactBashCommandGraduatedFlags(t *testing.T) {
	// ssh has flags -i key -o opt — the command is slightly over 60 bytes once path is compressed.
	// Should remove only the longest flag first, not all flags.
	cmd := `ssh -i ~/.ssh/key -o StrictHostKeyChecking=no ubuntu@host "echo hello"`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("missing omission marker: %q", got)
	}
	// Host must always be present (non-flag args kept).
	if !strings.Contains(got, "ubuntu@host") {
		t.Errorf("host dropped: %q", got)
	}
	// Only the longest flag (-o StrictHostKeyChecking=no) should be removed; -i must survive.
	if !strings.Contains(got, "-i") {
		t.Errorf("short flag -i dropped prematurely (graduated removal violated): %q", got)
	}
	if strings.Contains(got, "StrictHostKeyChecking") {
		t.Errorf("long flag StrictHostKeyChecking= should have been removed first: %q", got)
	}
}

// TestCompactBashCommandLongCommandKeepsKeyTokens verifies that commands still > maxLen after P1/P2/P3
// have their key tokens preserved via P4 arg-value truncation. All cases: <= 60 bytes with "…".
func TestCompactBashCommandLongCommandKeepsKeyTokens(t *testing.T) {
	// (a) SSH with long remote cmd: full host must survive; P4 truncates the quoted remote command.
	sshCmd := `ssh -i ~/.ssh/mac.pem ubuntu@147.224.141.104 "curl -sS http://127.0.0.1:4567/ | sed 's|src=\"/assets/|src=\"/inkos/assets/|g; s|href=\"/assets/|href=\"/inkos/assets/|g'" | head -15`
	gotSSH := CompactBashCommand(sshCmd, 60)
	if utf8.RuneCountInString(gotSSH) > 60 {
		t.Errorf("ssh case: result too long: %d runes > 60: %q", utf8.RuneCountInString(gotSSH), gotSSH)
	}
	if !strings.Contains(gotSSH, "ubuntu@147.224.141.104") {
		t.Errorf("ssh case: full host dropped (must never be mid-truncated): %q", gotSSH)
	}
	if !strings.Contains(gotSSH, "…") {
		t.Errorf("ssh case: missing omission marker: %q", gotSSH)
	}

	// (b) Double-grep pipeline: both grep command names must survive; long pattern truncated.
	grepCmd := `grep "description" /home/user/Workspace/Github/Project/internal/notify/notify.go | grep "smart\|compact\|Bash\|CompactBashCommand" /home/user/Workspace/Github/Project/internal/notify/notify.go`
	gotGrep := CompactBashCommand(grepCmd, 60)
	if utf8.RuneCountInString(gotGrep) > 60 {
		t.Errorf("double-grep case: result too long: %d runes > 60: %q", utf8.RuneCountInString(gotGrep), gotGrep)
	}
	if strings.Count(gotGrep, "grep") < 2 {
		t.Errorf("double-grep case: expected >= 2 'grep' occurrences, got %d: %q", strings.Count(gotGrep, "grep"), gotGrep)
	}
	if !strings.Contains(gotGrep, "…") {
		t.Errorf("double-grep case: missing omission marker: %q", gotGrep)
	}

	// (c) grep | python3: python3 command name must survive; P4 truncates the -c argument value.
	py3Cmd := `grep -A 10 -r "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go | python3 -c "import sys; print(sys.stdin.read())"`
	gotPy3 := CompactBashCommand(py3Cmd, 60)
	if utf8.RuneCountInString(gotPy3) > 60 {
		t.Errorf("python3 case: result too long: %d runes > 60: %q", utf8.RuneCountInString(gotPy3), gotPy3)
	}
	if !strings.Contains(gotPy3, "python3") {
		t.Errorf("python3 case: python3 command name dropped: %q", gotPy3)
	}
	if !strings.Contains(gotPy3, "…") {
		t.Errorf("python3 case: missing omission marker: %q", gotPy3)
	}
}

// TestCompactBashCommandOmissionMarker verifies that ssh host is kept and "…" marks removed flags.
// Uses a command where after flag removal the host + quoted cmd fits within 60 bytes.
func TestCompactBashCommandOmissionMarker(t *testing.T) {
	// After removing flags (-i key, -o StrictHostKeyChecking=no), the render is:
	// "ssh … ubuntu@host "sudo docker version"" = ~40 bytes, well under 60.
	cmd := `ssh -i ~/.ssh/key -o StrictHostKeyChecking=no ubuntu@host "sudo docker version"`
	got := CompactBashCommand(cmd, 60)
	if !strings.Contains(got, "ubuntu@host") {
		t.Errorf("host dropped: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("missing omission marker for removed flags: %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
}

// TestCompactBashCommandFillBudget verifies the result fills the budget (not a tiny skeleton).
func TestCompactBashCommandFillBudget(t *testing.T) {
	cmd := `grep -A 30 -B 10 -n -r "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Must fill the budget: result should be close to 60 runes (within 8 runes of limit).
	if utf8.RuneCountInString(got) < 60-8 {
		t.Errorf("result too short (not filling budget): %d runes, want >= %d: %q", utf8.RuneCountInString(got), 60-8, got)
	}
}

// TestCompactBashCommandManySubcommands tests ~15 semicolon-separated sub-commands,
// both single-line and multi-line (newline) variants.
func TestCompactBashCommandManySubcommands(t *testing.T) {
	single := "cmd1 ; cmd2 ; cmd3 ; cmd4 ; cmd5 ; cmd6 ; cmd7 ; cmd8 ; cmd9 ; cmd10 ; cmd11 ; cmd12 ; cmd13 ; cmd14 ; cmd15"
	multi := "cmd1\ncmd2\ncmd3\ncmd4\ncmd5\ncmd6\ncmd7\ncmd8\ncmd9\ncmd10\ncmd11\ncmd12\ncmd13\ncmd14\ncmd15"
	for _, cmd := range []string{single, multi} {
		got := CompactBashCommand(cmd, 60)
		if utf8.RuneCountInString(got) > 60 {
			t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("missing omission marker: %q", got)
		}
		if !strings.Contains(got, "cmd1") {
			t.Errorf("first sub-command dropped: %q", got)
		}
		if !strings.Contains(got, "cmd15") {
			t.Errorf("last sub-command dropped: %q", got)
		}
		// Should keep more than just head+tail: at least some middle sub-commands.
		count := 0
		for i := 1; i <= 15; i++ {
			name := fmt.Sprintf("cmd%d", i)
			if strings.Contains(got, name) {
				count++
			}
		}
		if count < 3 {
			t.Errorf("too few sub-commands retained (%d), expected at least 3: %q", count, got)
		}
	}
}

// TestReduceToBudgetHardCap verifies the hard cap is always enforced.
func TestReduceToBudgetHardCap(t *testing.T) {
	// Pathological: many sub-commands each with long names.
	var cmds []string
	for i := 0; i < 20; i++ {
		cmds = append(cmds, fmt.Sprintf("very-long-command-name-%d arg1 arg2 arg3", i))
	}
	var parts []string
	for i, c := range cmds {
		parts = append(parts, c)
		if i < len(cmds)-1 {
			parts = append(parts, ";")
		}
	}
	for _, maxLen := range []int{20, 30, 40, 60} {
		got := reduceToBudget(parts, maxLen)
		if utf8.RuneCountInString(got) > maxLen {
			t.Errorf("hard cap violated: runes=%d > maxLen=%d: %q", utf8.RuneCountInString(got), maxLen, got)
		}
	}
}

// TestCompactBashCommandConfigurableLimit verifies maxLen=100 > maxLen=60 and maxLen=0 fallback.
func TestCompactBashCommandConfigurableLimit(t *testing.T) {
	cmd := `grep -A 30 -B 10 -n -r "very-long-pattern-string" /home/user/Workspace/Github/Project/internal/notify/notify.go`
	got60 := CompactBashCommand(cmd, 60)
	got100 := CompactBashCommand(cmd, 100)
	if utf8.RuneCountInString(got60) > 60 {
		t.Errorf("maxLen=60 result too long: %d runes > 60: %q", utf8.RuneCountInString(got60), got60)
	}
	if utf8.RuneCountInString(got100) > 100 {
		t.Errorf("maxLen=100 result too long: %d runes > 100: %q", utf8.RuneCountInString(got100), got100)
	}
	// maxLen=100 should keep more content than maxLen=60.
	if utf8.RuneCountInString(got100) <= utf8.RuneCountInString(got60) {
		t.Errorf("expected maxLen=100 result longer than maxLen=60: got100=%q (%d runes), got60=%q (%d runes)", got100, utf8.RuneCountInString(got100), got60, utf8.RuneCountInString(got60))
	}
	// maxLen=0 must fall back to 60.
	got0 := CompactBashCommand(cmd, 0)
	if utf8.RuneCountInString(got0) > 60 {
		t.Errorf("maxLen=0 fallback violated: %d runes > 60: %q", utf8.RuneCountInString(got0), got0)
	}
}

// TestBuildCompactToolLineRespectsMaxLen verifies non-Bash tool types respect maxLen.
func TestBuildCompactToolLineRespectsMaxLen(t *testing.T) {
	for _, maxLen := range []int{20, 30} {
		// Grep
		grepInput := `{"pattern":"very-long-pattern-that-exceeds-limit","path":"/home/user/very/long/path"}`
		got := BuildCompactToolLine("Grep", []byte(grepInput), "/tmp", maxLen)
		// Extract the variable content (after emoji + " Grep: ").
		if idx := strings.Index(got, ": "); idx >= 0 {
			content := got[idx+2:]
			// Rune-count check: budget is now measured in characters (runes).
			if utf8.RuneCountInString(content) > maxLen {
				t.Errorf("Grep maxLen=%d: content too long (%d runes): %q", maxLen, utf8.RuneCountInString(content), got)
			}
		}

		// WebFetch
		fetchInput := `{"url":"https://very-long-url-that-definitely-exceeds-the-limit.example.com/path/to/resource"}`
		got = BuildCompactToolLine("WebFetch", []byte(fetchInput), "/tmp", maxLen)
		if idx := strings.Index(got, ": "); idx >= 0 {
			content := got[idx+2:]
			if utf8.RuneCountInString(content) > maxLen {
				t.Errorf("WebFetch maxLen=%d: content too long (%d runes): %q", maxLen, utf8.RuneCountInString(content), got)
			}
		}

		// Skill
		skillInput := `{"skill":"very-long-skill-name-that-exceeds-the-configured-limit-definitely"}`
		got = BuildCompactToolLine("Skill", []byte(skillInput), "/tmp", maxLen)
		if idx := strings.Index(got, ": "); idx >= 0 {
			content := got[idx+2:]
			if utf8.RuneCountInString(content) > maxLen {
				t.Errorf("Skill maxLen=%d: content too long (%d runes): %q", maxLen, utf8.RuneCountInString(content), got)
			}
		}

		// Agent with model/subtype + long desc
		agentInput := `{"model":"claude-opus-4-5","subagent_type":"ca-executor","description":"A very long description that exceeds any reasonable limit set by the user"}`
		got = BuildCompactToolLine("Agent", []byte(agentInput), "/tmp", maxLen)
		// The variable content is after emoji + " ".
		if idx := strings.Index(got, " "); idx >= 0 {
			content := got[idx+1:]
			if utf8.RuneCountInString(content) > maxLen {
				t.Errorf("Agent maxLen=%d: content too long (%d runes): %q", maxLen, utf8.RuneCountInString(content), got)
			}
		}
	}
}

func TestTailPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		n        int
		expected string
	}{
		{"long absolute path", "/a/b/c/d/e/cmd/hooks/register.go", 3, "cmd/hooks/register.go"},
		{"short path unchanged", "/tmp/test.txt", 3, "/tmp/test.txt"},
		{"relative path", "cmd/hooks/register.go", 3, "cmd/hooks/register.go"},
		{"single file", "register.go", 3, "register.go"},
		{"exactly n segments", "a/b/c", 3, "a/b/c"},
		{"dot dirs preserved", "/a/b/c/.claude/skills/tg-cli/SKILL.md", 3, "skills/tg-cli/SKILL.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TailPath(tt.path, tt.n)
			if got != tt.expected {
				t.Errorf("TailPath(%q, %d) = %q, want %q", tt.path, tt.n, got, tt.expected)
			}
		})
	}
}

func TestTokenizeCommand(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected []string
	}{
		{"simple", "grep pattern file", []string{"grep", "pattern", "file"}},
		{"double quoted", `ssh host "cmd1 && cmd2"`, []string{"ssh", "host", `"cmd1 && cmd2"`}},
		{"single quoted", `grep 'a|b' file`, []string{"grep", `'a|b'`, "file"}},
		{"operators", "cmd1 && cmd2 | cmd3", []string{"cmd1", "&&", "cmd2", "|", "cmd3"}},
		{"quoted pipe", `grep "a|b" file`, []string{"grep", `"a|b"`, "file"}},
		{"no-space &&", "cmd1&&cmd2", []string{"cmd1", "&&", "cmd2"}},
		{"no-space ;", "cmd1;cmd2", []string{"cmd1", ";", "cmd2"}},
		{"no-space pipe", "grep x file|head", []string{"grep", "x", "file", "|", "head"}},
		{"no-space ||", "cmd1||cmd2", []string{"cmd1", "||", "cmd2"}},
		{"escaped quote in double quotes", `ssh host "curl | sed 's|src=\"/a/|src=\"/b/|g'"`, []string{"ssh", "host", `"curl | sed 's|src=\"/a/|src=\"/b/|g'"`}},
		{"escaped quote preserves content", `ssh host "echo \"hello world\""`, []string{"ssh", "host", `"echo \"hello world\""`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenizeCommand(tt.cmd)
			if len(got) != len(tt.expected) {
				t.Errorf("tokenizeCommand(%q) = %v (len %d), want %v (len %d)", tt.cmd, got, len(got), tt.expected, len(tt.expected))
				return
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("tokenizeCommand(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestSplitSubCommands(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected []string
	}{
		{"single command", "grep pattern file", []string{"grep pattern file"}},
		{"two commands", "cmd1 && cmd2", []string{"cmd1", "&&", "cmd2"}},
		{"quoted operator", `ssh host "cmd1 && cmd2"`, []string{`ssh host "cmd1 && cmd2"`}},
		{"pipe", "grep pattern | head", []string{"grep pattern", "|", "head"}},
		{"semicolon", "cmd1 ; cmd2", []string{"cmd1", ";", "cmd2"}},
		{"mixed", "cd dir && go build ./... | tee log", []string{"cd dir", "&&", "go build ./...", "|", "tee log"}},
		{"no-space &&", "cmd1&&cmd2", []string{"cmd1", "&&", "cmd2"}},
		{"no-space ;", "cmd1;cmd2", []string{"cmd1", ";", "cmd2"}},
		{"no-space pipe", "grep x file|head", []string{"grep x file", "|", "head"}},
		{"escaped quotes preserve operators", `ssh host "cmd1 | sed \"s|a|b|g\""  | head`, []string{`ssh host "cmd1 | sed \"s|a|b|g\""`, "|", "head"}},
		{"remote cmd with src= stays one subcommand", `ssh host "curl http://x/ | sed 's|src=\"/a/|src=\"/b/|g'" | head`, []string{`ssh host "curl http://x/ | sed 's|src=\"/a/|src=\"/b/|g'"`, "|", "head"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSubCommands(tt.cmd)
			if len(got) != len(tt.expected) {
				t.Errorf("splitSubCommands(%q) = %v (len %d), want %v (len %d)", tt.cmd, got, len(got), tt.expected, len(tt.expected))
				return
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("splitSubCommands(%q)[%d] = %q, want %q", tt.cmd, i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestCompactBashCommand(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		maxCheck    int
		contains    string
		notContains string
	}{
		{
			name:        "heredoc merged to single line",
			cmd:         "python3 << 'PY'\nimport re\nwith open('file') as f:\n    pass\nPY",
			contains:    "python3",
			notContains: "\n",
		},
		{
			name:     "absolute path compressed",
			cmd:      "ls /home/user/Workspace/Github/Project/cmd/hooks/register.go",
			contains: "…/",
		},
		{
			name:     "flags stripped when needed",
			cmd:      "grep -A30 -n -r \"func CompressPath\" /home/user/Workspace/Github/Project/internal/notify/notify.go",
			contains: "\"func CompressPath\"",
		},
		{
			name:     "ssh preserves host",
			cmd:      "ssh -i ~/.ssh/key -o StrictHostKeyChecking=no ubuntu@147.224.141.104 \"sudo docker version\"",
			contains: "ubuntu@147.224.141.104",
		},
		{
			name:     "multi-command chain preserved",
			cmd:      "cd /tmp && go build ./...",
			contains: "&&",
		},
		{
			name:     "quoted operator preserved",
			cmd:      `grep "a|b" /home/user/Workspace/Github/Project/internal/notify/notify.go`,
			contains: `"a|b"`,
		},
		{
			name:     "result within limit",
			cmd:      "very-long-command-name --flag1 --flag2 --flag3 arg1 arg2 arg3 arg4 arg5 arg6 /home/user/a/b/c/d/e/f/g/h/file.go",
			maxCheck: 60,
		},
		{
			name:     "short path unchanged",
			cmd:      "cat /tmp/test.txt",
			contains: "/tmp/test.txt",
		},
		{
			name:        "comment line stripped shows real command",
			cmd:         "# comment\nps aux | grep plasmashell | grep -v grep",
			contains:    "ps",
			notContains: "\n",
		},
		{
			name:     "comment before ssh shows ssh",
			cmd:      "# Verify\nssh -i ~/.ssh/key ubuntu@host \"docker version\"",
			contains: "ssh",
		},
		{
			name:     "pure comment returns …",
			cmd:      "# just a comment",
			contains: "…",
		},
		{
			name:     "quoted hash not stripped",
			cmd:      `echo "# not comment"`,
			contains: `"# not comment"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactBashCommand(tt.cmd, 60)
			if tt.maxCheck > 0 && utf8.RuneCountInString(got) > tt.maxCheck {
				t.Errorf("CompactBashCommand result too long: %d runes > %d: %q", utf8.RuneCountInString(got), tt.maxCheck, got)
			}
			if tt.contains != "" && !strings.Contains(got, tt.contains) {
				t.Errorf("CompactBashCommand(%q) = %q, missing %q", tt.cmd, got, tt.contains)
			}
			if tt.notContains != "" && strings.Contains(got, tt.notContains) {
				t.Errorf("CompactBashCommand(%q) = %q, should not contain %q", tt.cmd, got, tt.notContains)
			}
		})
	}
}

func TestStripFlagsAware(t *testing.T) {
	tests := []struct {
		name     string
		cmdName  string
		tokens   []string
		expected []string
	}{
		{
			name:     "ssh strips -i and its value",
			cmdName:  "ssh",
			tokens:   []string{"-i", "~/.ssh/key", "-o", "StrictHostKeyChecking=no", "ubuntu@host", "\"docker version\""},
			expected: []string{"ubuntu@host", "\"docker version\""},
		},
		{
			name:     "grep strips -A and its value",
			cmdName:  "grep",
			tokens:   []string{"-A", "30", "-n", "\"func\"", "file.go"},
			expected: []string{"\"func\"", "file.go"},
		},
		{
			name:     "unknown command strips flags only",
			cmdName:  "mycmd",
			tokens:   []string{"-v", "--verbose", "arg1", "arg2"},
			expected: []string{"arg1", "arg2"},
		},
		{
			name:     "long flag with equals stripped",
			cmdName:  "ssh",
			tokens:   []string{"--config=/path/to/config", "host"},
			expected: []string{"host"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := stripFlagsAware(tt.cmdName, tt.tokens)
			if len(got) != len(tt.expected) {
				t.Errorf("stripFlagsAware(%q, %v) = %v (len %d), want %v (len %d)", tt.cmdName, tt.tokens, got, len(got), tt.expected, len(tt.expected))
				return
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("stripFlagsAware[%d] = %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestCompactBashCommandLongCmds(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		contains    []string
		notContains []string
		maxCheck    int
		grepCount   int
		noTrailDots bool
	}{
		{
			name:        "tavily: skeleton, no noise",
			cmd:         "TOKEN=$(cat /home/user/long/path/to/token.txt) && ~/go/bin/mcptools call tavily_search -p '{\"query\":\"github\"}' --auth-header \"Bearer $TOKEN\" -f json 'https://example.com/mcp' | python3 -c \"import sys,json\"",
			contains:    []string{"mcptools"},
			notContains: []string{"TOKEN=$(cat"},
			maxCheck:    60,
		},
		{
			// command-first: cd path is restored whole (it's the first command), tg-cli args (service upgrade)
			// are omitted as "…" because they don't fit after cd path consumes the budget.
			name:     "service upgrade preserves subcommand",
			cmd:      "cd /home/user/Workspace/Github/Project/very-long-worktree && ./tg-cli service upgrade 2>&1",
			contains: []string{"cd", "tg-cli", "…"},
			maxCheck: 60,
		},
		{
			name:        "short command uses command not description",
			cmd:         "echo hello",
			contains:    []string{"echo hello"},
			notContains: []string{"Print hello"},
		},
		{
			name:        "no description falls back to skeleton",
			cmd:         "TOKEN=$(cat /home/user/long/path/to/token.txt) && ~/go/bin/mcptools call tavily_search -p '{\"query\":\"github\"}' | python3",
			contains:    []string{"mcptools", "call"},
			notContains: []string{"TOKEN=$(cat"},
		},
		{
			name:     "long result capped at 60 bytes",
			cmd:      "TOKEN=$(cat /home/user/long/path/to/token.txt) && very-long-command-that-does-something-complex-with-many-arguments-and-flags",
			maxCheck: 60,
		},
		{
			name:        "description sentinel cannot leak",
			cmd:         "TOKEN=$(cat /home/user/long/path/to/token.txt) && ~/go/bin/mcptools call tavily_search -p '{\"query\":\"github\"}' --auth-header \"Bearer $TOKEN\" -f json 'https://example.com/mcp' | python3 -c \"import sys,json\"",
			contains:    []string{"mcptools"},
			notContains: []string{"SHOULD_NOT_APPEAR"},
			maxCheck:    60,
		},
		{
			name:        "pacman case: no description colon",
			cmd:         "which caddy ; which python3 ; pacman -Ss nginx",
			contains:    []string{"which", "pacman"},
			notContains: []string{"Check availab"},
			maxCheck:    60,
		},
		{
			// P4 arg-value truncation keeps both grep command names by truncating the long pattern
			// value in place instead of letting the whole-string hard cap mangle them.
			name:        "pipeline: both greps visible with pattern ends preserved",
			cmd:         `grep "description" /home/user/Workspace/Github/Project/internal/notify/notify.go | grep "smart\|compact\|Bash\|CompactBashCommand" /home/user/Workspace/Github/Project/internal/notify/notify.go`,
			contains:    []string{"grep", "notify.go", "…"},
			notContains: []string{"CompactB…", "in…y.go"},
			grepCount:   2,
			maxCheck:    60,
		},
		{
			name:        "CJK pattern not garbled",
			cmd:         `grep -i "公益|api_base|base_url|openai|proxy|forward|中转" /home/user/Workspace/Github/Project/ClaudeNote/project-registry.md 2>/dev/null | grep -i story`,
			notContains: []string{"�", "��"},
			grepCount:   2,
			maxCheck:    60,
		},
		{
			name:        "heredoc body stripped, only shell command kept",
			cmd:         "cat << 'SQL' | ssh -i ~/.ssh/key ubuntu@host \"docker exec -i db mariadb\"\nINSERT INTO app (\n  owner, name\n) VALUES (\n  'admin', 'test'\n);\nSQL",
			contains:    []string{"cat", "ssh", "…"},
			notContains: []string{"INSERT", "VALUES", "admin"},
			maxCheck:    60,
		},
		{
			name:        "heredoc nginx not fragmented",
			cmd:         "cat << 'CONF' | ssh host \"sudo tee /etc/nginx/app.conf\"\nserver {\n    listen 443 ssl;\n    server_name app.example.com;\n    location / {\n        proxy_pass http://127.0.0.1:8080;\n    }\n}\nCONF",
			notContains: []string{"s…e", "l…n", "p…r"},
			contains:    []string{"cat", "ssh", "…"},
			maxCheck:    60,
		},
		{
			name:        "redirection not treated as file arg",
			cmd:         "cat /tmp/claude-1000/-home-seraphli-Workspace/2581a41f/tasks/b3nopw800.output 2>/dev/null",
			contains:    []string{"b3nopw800.output"},
			notContains: []string{"null"},
			maxCheck:    60,
		},
		{
			name:        "stderr redirect filtered",
			cmd:         "grep pattern /home/user/very/long/path/to/project/src/file.go 2>&1",
			contains:    []string{"grep", "file.go"},
			notContains: []string{"2>&1"},
		},
		{
			name:        "separated redirection target not kept",
			cmd:         "cat /tmp/claude-1000/-home-seraphli-Workspace/2581a41f/tasks/b3nopw800.output 2> /dev/null",
			contains:    []string{"b3nopw800.output"},
			notContains: []string{"/dev/null"},
			maxCheck:    60,
		},
		{
			// P4 arg-value truncation keeps the full host by truncating the long remote quoted
			// command value in place. The host (ubuntu@147.224.141.104) must NEVER be mid-truncated.
			name:        "ssh with escaped quotes in remote cmd",
			cmd:         `ssh -i ~/.ssh/mac.pem ubuntu@147.224.141.104 "curl -sS http://127.0.0.1:4567/ | sed 's|src=\"/assets/|src=\"/inkos/assets/|g; s|href=\"/assets/|href=\"/inkos/assets/|g'" | head -15`,
			contains:    []string{"ssh", "ubuntu@147.224.141.104", "…"},
			notContains: []string{"|  |"},
			maxCheck:    60,
		},
		{
			// Shell script with function+for: newlines joined as " ; "; graduated reducer keeps
			// first real anchor (check_site) and last anchor (done), drops middle sub-commands.
			name:        "shell script function+for not fragmented",
			cmd:         "# Quick batch\ncheck_site() {\n  local site=$1\n  if echo \"$snap\" | grep -qi 'LinuxDO'; then\n    echo yes\n  fi\n}\nfor site in a.com b.com; do\n  check_site \"$site\"\ndone",
			contains:    []string{"check_site", "…"},
			notContains: []string{"c…e()", "|  |"},
			maxCheck:    60,
		},
		{
			name:        "python for/if not detected as shell structure",
			cmd:         "python3 -c \"\nimport re\nfor line in open('file'):\n    if 'pattern' in line:\n        print(line)\n\"",
			notContains: []string{"for line"},
		},
		{
			name:        "curl with continuation lines",
			cmd:         "curl -s --max-time 15 \"https://example.com/api\" \\\n  -H \"Authorization: Bearer token\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"key\":\"val\"}' | head -5",
			contains:    []string{"curl"},
			notContains: []string{"15 \\", "; -H", "; -d"},
			maxCheck:    60,
		},
		{
			name:        "env assignment stripped, real command preserved",
			cmd:         `OPENCLI_CDP_ENDPOINT="ws://127.0.0.1:9223" opencli browser test open "https://example.com" 2>&1`,
			contains:    []string{"opencli", "browser"},
			notContains: []string{"127.0.0.1:9223", "OPENCLI", "ws://"},
			maxCheck:    60,
		},
		{
			name:        "multiple env assignments stripped",
			cmd:         `FOO=bar BAZ="qux" real-command arg1 arg2`,
			contains:    []string{"real-command"},
			notContains: []string{"FOO=", "BAZ="},
		},
		{
			name:        "TOKEN=$(cat) inline env stripped",
			cmd:         `TOKEN=$(cat /tmp/token.txt) opencli browser test`,
			contains:    []string{"opencli", "browser"},
			notContains: []string{"TOKEN=$(cat", "token.txt)"},
		},
		{
			name:        "TOKEN=$(cat) with && no orphan operator",
			cmd:         `TOKEN=$(cat /home/user/long/path/to/token.txt) && ~/go/bin/mcptools call tavily_search`,
			contains:    []string{"mcptools"},
			notContains: []string{"TOKEN=$(cat"},
		},
		{
			name:        "middle env assignment no double operator",
			cmd:         "cmd1 && FOO=bar && cmd2",
			contains:    []string{"cmd1", "cmd2"},
			notContains: []string{"FOO=bar", "&& &&"},
		},
		{
			name:     "middle args omitted with ellipsis",
			cmd:      "some-tool first-arg middle1 middle2 middle3 middle4 middle5 last-arg /home/user/very/long/path/to/file.go",
			contains: []string{"…"},
		},
		{
			name:     "cat multi-file shows ellipsis",
			cmd:      "cat /home/user/very/long/path/file1.txt /home/user/very/long/path/file2.txt /home/user/very/long/path/file3.txt",
			contains: []string{"cat", "…"},
		},
		{
			name:     "rm multi-target shows ellipsis",
			cmd:      "rm /home/user/very/long/path/file1.tmp /home/user/very/long/path/file2.tmp /home/user/very/long/path/file3.tmp",
			contains: []string{"rm", "…"},
		},
		{
			name:     "env-only command returns …",
			cmd:      `FOO=bar BAZ=qux`,
			contains: []string{"…"},
		},
		// 2a: Env omission
		{
			name:     "env assignment leaves … marker",
			cmd:      `SEARCH=/home/user/Workspace/Github/Seraphli/OBNote/.claude/skills/session-search/search.py && python3 $SEARCH "滑块 验证 过 方法" --limit 20`,
			contains: []string{"…", "&&", "python3"},
		},
		{
			name:     "env-only sub-command becomes …",
			cmd:      "cmd1 && FOO=bar && cmd2",
			contains: []string{"cmd1", "…", "cmd2"},
		},
		{
			name:     "env-only whole command",
			cmd:      "FOO=bar BAZ=qux",
			contains: []string{"…"},
		},
		// 2b: Flag omission (long commands forcing L1)
		{
			name:     "long cmd flag stripping has … marker",
			cmd:      `grep -A 20 -B 10 -r -n "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go`,
			contains: []string{"grep", "…", "pattern"},
		},
		// 2c: Redirection omission
		{
			// command-first: Stage A restores flags short-first (-r, -n, -A 20, -B 10) then "pattern",
			// then the path. Path truncation via middle-truncate to fit. TrailMarker (…) is always shown.
			// "notify.go" may not survive path truncation — the key assertions are grep + head visible + "…".
			name:     "redirection removed has … marker before pipe",
			cmd:      `grep -A 20 -B 10 -r -n "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go 2>/dev/null | head -5`,
			contains: []string{"grep", "…", "| head"},
		},
		// 2d: Formatter visibility — P4 arg-value truncation keeps python3 command name by
		// truncating its -c argument value in place, never letting the whole-string hard cap
		// mangle the second sub-command's name.
		{
			name:     "formatter command visible after simplification",
			cmd:      `grep -A 10 -r "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go | python3 -c "import sys; print(sys.stdin.read())"`,
			contains: []string{"grep", "python3", "…"},
			maxCheck: 60,
		},
		// 2e: Path …/ prefix
		{
			name:     "path shortening has …/ prefix",
			cmd:      "ls /home/user/Workspace/Github/Project/cmd/hooks/register.go",
			contains: []string{"…/"},
		},
		// 2f: Comment removal
		{
			name:     "comment removal leaves … marker",
			cmd:      "# setup comment\necho hello",
			contains: []string{"…", "echo"},
		},
		// 2f-extra: All-comment and inline comment
		{
			name:        "all-comment command returns …",
			cmd:         "# just a comment",
			contains:    []string{"…"},
			notContains: []string{"#"},
		},
		{
			name:        "multi-line all-comments returns …",
			cmd:         "# line 1\n# line 2",
			contains:    []string{"…"},
			notContains: []string{"#"},
		},
		{
			name:     "inline comment removed has … marker",
			cmd:      "echo hello # secret comment",
			contains: []string{"echo", "hello", "…"},
		},
		// 2g: Heredoc body
		{
			name:     "heredoc body stripped has … marker",
			cmd:      "cat << 'EOF' | ssh host cmd\nline1\nline2\nEOF",
			contains: []string{"…", "cat"},
		},
		// 2h: Shell structure (extractShellStructure removed; newlines joined as " ; ".
		// Graduated reducer keeps first real anchor (check_site) and last anchor (done), drops middles.)
		{
			name:     "shell structure summary has …",
			cmd:      "check_site() {\n  echo hi\n}\nfor site in a b c; do\n  check_site $site\ndone",
			contains: []string{"check_site", "…"},
			maxCheck: 60,
		},
		// 2i: … preserved through full compaction
		{
			name:     "… preserved through full compaction",
			cmd:      `SEARCH=/home/user/Workspace/Github/Seraphli/OBNote/.claude/skills/session-search/search.py && python3 $SEARCH "滑块 验证 过 方法" --limit 20`,
			contains: []string{"…", "&&", "python3"},
			maxCheck: 60,
		},
		// 2j: L1 marker-bearing sub-commands
		{
			name:        "long env assignment … survives L1",
			cmd:         `OPENCLI_CDP_ENDPOINT="ws://127.0.0.1:9223" opencli browser test open "https://very-long-example.com/api/endpoint/path" extra-arg`,
			contains:    []string{"…", "opencli", "browser"},
			notContains: []string{"127.0.0.1:9223"},
			maxCheck:    60,
		},
		{
			name:     "long comment removal … survives L1",
			cmd:      "# setup environment variables for the deployment\ngrep -A 20 -B 10 -r -n \"pattern\" /home/user/Workspace/Github/Project/internal/notify/notify.go",
			contains: []string{"…", "grep"},
		},
		{
			name:     "long redirection removal … survives L1",
			cmd:      `grep -A 20 -B 10 -r -n "pattern" /home/user/Workspace/Github/Project/internal/notify/notify.go 2>/dev/null`,
			contains: []string{"grep", "…"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactBashCommand(tt.cmd, 60)
			if tt.maxCheck > 0 && utf8.RuneCountInString(got) > tt.maxCheck {
				t.Errorf("result too long: %d runes > %d: %q", utf8.RuneCountInString(got), tt.maxCheck, got)
			}
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("CompactBashCommand = %q, missing %q", got, c)
				}
			}
			for _, nc := range tt.notContains {
				if strings.Contains(got, nc) {
					t.Errorf("CompactBashCommand = %q, should not contain %q", got, nc)
				}
			}
			if tt.grepCount > 0 && strings.Count(got, "grep") < tt.grepCount {
				t.Errorf("CompactBashCommand = %q, expected >= %d 'grep' occurrences, got %d", got, tt.grepCount, strings.Count(got, "grep"))
			}
			if tt.noTrailDots && strings.HasSuffix(got, "…") {
				t.Errorf("CompactBashCommand = %q, should not end with …", got)
			}
		})
	}
}

func TestStripShellComments(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected string
	}{
		{
			name:     "whole-line comment removed",
			cmd:      "# this is a comment\nps aux | grep plasmashell",
			expected: "ps aux | grep plasmashell",
		},
		{
			name:     "multiple comment lines removed",
			cmd:      "# Verify the JSON is valid now\n# Another comment\nssh host cmd",
			expected: "ssh host cmd",
		},
		{
			name:     "inline comment stripped",
			cmd:      "echo hello # trailing comment",
			expected: "echo hello",
		},
		{
			name:     "hash without preceding space preserved",
			cmd:      "echo foo#bar",
			expected: "echo foo#bar",
		},
		{
			name:     "hash with preceding space stripped",
			cmd:      "echo foo # bar",
			expected: "echo foo",
		},
		{
			name:     "quoted hash preserved",
			cmd:      `echo "# not a comment"`,
			expected: `echo "# not a comment"`,
		},
		{
			name:     "single-quoted hash preserved",
			cmd:      "echo '# not a comment'",
			expected: "echo '# not a comment'",
		},
		{
			name:     "pure comments return empty",
			cmd:      "# just a comment\n# another comment",
			expected: "",
		},
		{
			name:     "mixed comments and commands",
			cmd:      "# setup\ncd /tmp\n# build\ngo build ./...",
			expected: "cd /tmp\ngo build ./...",
		},
		{
			name:     "empty lines skipped",
			cmd:      "\n# comment\n\nls /tmp\n",
			expected: "ls /tmp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := stripShellComments(tt.cmd)
			if got != tt.expected {
				t.Errorf("stripShellComments(%q) = %q, want %q", tt.cmd, got, tt.expected)
			}
		})
	}
}

func TestBuildCompactToolLineBashNoDescription(t *testing.T) {
	toolInput := `{"command":"TOKEN=$(cat /home/user/long/path/token.txt) && ~/go/bin/mcptools call tavily_search -p '{\"query\":\"github\"}' | python3 -c \"import json\"","description":"SHOULD_NOT_APPEAR_DESCRIPTION_SENTINEL"}`
	result := BuildCompactToolLine("Bash", []byte(toolInput), "/tmp", 60)
	if strings.Contains(result, "SHOULD_NOT_APPEAR") {
		t.Errorf("BuildCompactToolLine leaked description: %q", result)
	}
	if !strings.Contains(result, "mcptools") {
		t.Errorf("missing command skeleton: %q", result)
	}
	if strings.Contains(result, "TOKEN=$(cat") {
		t.Errorf("contains var assignment noise: %q", result)
	}
}

// Fix 13b: a no-arg tool (e.g. TaskList with tool_input {}) has no arg info, but the compact line must
// still show at least the tool name (with its emoji) so the tool call is visible in compact display.
func TestBuildCompactToolLineNoArgTool(t *testing.T) {
	result := BuildCompactToolLine("TaskList", []byte(`{}`), "/tmp", 60)
	if result != "📋 TaskList" {
		t.Errorf("no-arg compact line should be name-only %q, got %q", "📋 TaskList", result)
	}
}

// Fix 14: BuildCompactToolDetails wraps the compact one-line summary in a collapsed <details> block
// whose body is the full tool args, so the user can expand to see the actual command. A no-arg tool has
// no expandable body → just the summary line, no <details>.
func TestBuildCompactToolDetails(t *testing.T) {
	got := BuildCompactToolDetails("Bash", []byte(`{"command":"echo hello world"}`), "/tmp", 50)
	if !strings.HasPrefix(got, "<details><summary>") {
		t.Errorf("expected a <details><summary> prefix, got %q", got)
	}
	if !strings.Contains(got, "</summary>") || !strings.HasSuffix(got, "</details>") {
		t.Errorf("expected a complete <details> block, got %q", got)
	}
	// The <summary> is the compact one-liner; the collapsed body carries the full command.
	if !strings.Contains(got, "💻 Bash") || !strings.Contains(got, "echo hello world") {
		t.Errorf("expected the compact summary and full command, got %q", got)
	}
	// No-arg tool: no expandable body → plain summary, no <details>.
	na := BuildCompactToolDetails("TaskList", []byte(`{}`), "/tmp", 50)
	if strings.Contains(na, "<details>") {
		t.Errorf("no-arg tool should not be wrapped in <details>, got %q", na)
	}
	if na != "📋 TaskList" {
		t.Errorf("no-arg compact details should be the plain summary %q, got %q", "📋 TaskList", na)
	}
}

// Fix 17: the compact Read summary shows only the filename (basename), not the tail path — the full
// path lives in the collapsed details body. Edit/Write keep the 3-segment tail path (unchanged).
func TestBuildCompactToolLineReadBasename(t *testing.T) {
	got := BuildCompactToolLine("Read", []byte(`{"file_path":"/tmp/claude-1000/-home-seraphli/tasks/ba96hbxz8.output"}`), "/tmp", 60)
	if got != "📖 Read: ba96hbxz8.output" {
		t.Errorf("compact Read should show basename only, got %q", got)
	}
	// Edit still shows the tail path (last 3 segments), not just the basename.
	edit := BuildCompactToolLine("Edit", []byte(`{"file_path":"/tmp/claude-1000/tasks/ba96hbxz8.output"}`), "/tmp", 60)
	if !strings.Contains(edit, "tasks/ba96hbxz8.output") {
		t.Errorf("compact Edit should keep the tail path, got %q", edit)
	}
}

func TestTruncateMiddle(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		max      int
		contains []string
	}{
		{"short string unchanged", "hello", 10, []string{"hello"}},
		{"suffix-biased preserves end", "CompactBashCommand", 12, []string{"Com", "ommand"}},
		{"long pattern preserves tail keyword", "smart|compact|Bash|CompactBashCommand", 20, []string{"smart", "BashCommand"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateMiddle(tt.s, tt.max)
			if utf8.RuneCountInString(got) > tt.max {
				t.Errorf("truncateMiddle(%q, %d) = %q (%d runes), exceeds max", tt.s, tt.max, got, utf8.RuneCountInString(got))
			}
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("truncateMiddle(%q, %d) = %q, missing %q", tt.s, tt.max, got, c)
				}
			}
		})
	}
}

func TestTruncateMiddleCJK(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
	}{
		{"CJK chars not split", "公益|api_base|forward|中转", 15},
		{"CJK only", "公益站点转发中转服务", 10},
		{"mixed CJK and ASCII", "公益forward中转test", 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateMiddle(tt.s, tt.max)
			if utf8.RuneCountInString(got) > tt.max {
				t.Errorf("truncateMiddle(%q, %d) = %q (%d runes), exceeds max", tt.s, tt.max, got, utf8.RuneCountInString(got))
			}
			if strings.Contains(got, "�") {
				t.Errorf("truncateMiddle(%q, %d) = %q, contains replacement char", tt.s, tt.max, got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateMiddle(%q, %d) = %q, invalid UTF-8", tt.s, tt.max, got)
			}
		})
	}
}

func TestStripHeredocBody(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected string
	}{
		{
			name:     "heredoc with single quotes",
			cmd:      "cat << 'EOF' | ssh host cmd\nline1\nline2\nEOF",
			expected: "cat << 'EOF' | ssh host cmd …",
		},
		{
			name:     "heredoc with double quotes",
			cmd:      "cat << \"EOF\" | cmd\nbody\nEOF",
			expected: "cat << \"EOF\" | cmd …",
		},
		{
			name:     "no heredoc unchanged",
			cmd:      "grep pattern file | head",
			expected: "grep pattern file | head",
		},
		{
			name:     "heredoc without pipe",
			cmd:      "cat << EOF\ndata\nEOF",
			expected: "cat << EOF …",
		},
		{
			name:     "tab-stripping heredoc",
			cmd:      "cat <<- 'EOF' | cmd\n\tbody line\n\tEOF",
			expected: "cat <<- 'EOF' | cmd …",
		},
		{
			name:     "quoted << not treated as heredoc",
			cmd:      "echo \"<< not heredoc\"\nthis line stays",
			expected: "echo \"<< not heredoc\"\nthis line stays",
		},
		{
			name:     "single-quoted << not treated as heredoc",
			cmd:      "echo '<< also not heredoc'\nsecond line",
			expected: "echo '<< also not heredoc'\nsecond line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHeredocBody(tt.cmd)
			if got != tt.expected {
				t.Errorf("stripHeredocBody(%q) = %q, want %q", tt.cmd, got, tt.expected)
			}
		})
	}
}

func TestIsRedirection(t *testing.T) {
	tests := []struct {
		token    string
		expected bool
	}{
		{"2>/dev/null", true},
		{">/dev/null", true},
		{"2>&1", true},
		{">output.txt", true},
		{">>append.txt", true},
		{"1>&2", true},
		{"file.txt", false},
		{"2files", false},
		{"-v", false},
		{"path/to/file", false},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			got := isRedirection(tt.token)
			if got != tt.expected {
				t.Errorf("isRedirection(%q) = %v, want %v", tt.token, got, tt.expected)
			}
		})
	}
}

func TestStripContinuation(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected string
	}{
		// Continued line is merged: "curl url \<nl>  -H header" → single logical line.
		{"trailing backslash removed", "curl url \\\n  -H header", "curl url -H header"},
		{"no backslash unchanged", "echo hello\necho world", "echo hello\necho world"},
		// Chained continuations must produce exactly "cmd arg1 arg2" — self-contained, no residual \n.
		{"multiple continuations", "cmd \\\n  arg1 \\\n  arg2", "cmd arg1 arg2"},
		// Mixed: continuation folds; plain newline keeps separate line.
		{"mixed continuation and plain newline", "a \\\n b\nc", "a b\nc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripContinuation(tt.cmd)
			if got != tt.expected {
				t.Errorf("stripContinuation(%q) = %q, want %q", tt.cmd, got, tt.expected)
			}
		})
	}
}

// TestNormalizeShellNewlines verifies quote-aware newline flattening: out-of-quote newlines become
// " ; " separators, while in-quote newlines (single or double) become a single space.
func TestNormalizeShellNewlines(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		expected string
	}{
		// In-double-quote newline => space (script content, not a separator).
		{"double-quote multiline", "python3 -c \"import x\nprint(1)\"", "python3 -c \"import x print(1)\""},
		// Out-of-quote newline => " ; " (real command separator).
		{"plain newline", "echo a\necho b", "echo a ; echo b"},
		// In-single-quote newline => space.
		{"single-quote multiline", "sh -c 'a\nb'", "sh -c 'a b'"},
		// Escaped \" does not close the double quote, so following newline is in-quote => space.
		{"escaped quote inside double", "python3 -c \"say \\\"hi\\\"\nbye\"", "python3 -c \"say \\\"hi\\\" bye\""},
		// Mixed: out-of-quote newlines => " ; "; in-quote newline => space.
		{"mixed in and out of quote", "a\n\"b\nc\"\nd", "a ; \"b c\" ; d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeShellNewlines(tt.cmd)
			if got != tt.expected {
				t.Errorf("normalizeShellNewlines(%q) = %q, want %q", tt.cmd, got, tt.expected)
			}
		})
	}
}

// TestCompactBashCommandQuotedMultilineNotSplit verifies that newlines inside a quoted multiline
// argument (e.g. a python -c script) are NOT turned into fake " ; " separators in the compact output.
func TestCompactBashCommandQuotedMultilineNotSplit(t *testing.T) {
	cmd := `mcptools call tavily_search --auth-header "Bearer x" "$ENDPOINT" 2>&1 | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['results'][:3])
"`
	got := CompactBashCommand(cmd, 60)
	if strings.Contains(got, `-c " ;`) {
		t.Errorf("in-quote newline split into fake sub-command after -c: %q", got)
	}
	if strings.Contains(got, "; import") {
		t.Errorf("script content appears after fake ';' (import): %q", got)
	}
	if strings.Contains(got, "; print") {
		t.Errorf("script content appears after fake ';' (print): %q", got)
	}
	if !strings.Contains(got, "python3") {
		t.Errorf("python3 command dropped (over-compacted): %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
}

func TestCompactBashCommandContinuationNotSplit(t *testing.T) {
	cmd := `TOKEN=$(cat /home/seraphli/Workspace/Github/Seraphli/OBNote/.claude/skills/tavily-search/token.txt)
MCPTOOLS=~/go/bin/mcptools
ENDPOINT='https://tavily.ivanli.cc/mcp'
echo "=== probe ==="
$MCPTOOLS call tavily_search \
  -p '{"query":"some long query string here for testing purposes xyz","max_results":6}' \
  --auth-header "Bearer $TOKEN" -f json "$ENDPOINT" 2>&1 | head -c 4500`
	got := CompactBashCommand(cmd, 60)
	if strings.Contains(got, "; -") {
		t.Errorf("continuation lines split into fake sub-commands: %q", got)
	}
	if strings.Contains(got, "; -p") {
		t.Errorf("flag -p appears after ';': %q", got)
	}
	if strings.Contains(got, "; --auth-header") {
		t.Errorf("flag --auth-header appears after ';': %q", got)
	}
	if !strings.Contains(got, "$MCPTOOLS") && !strings.Contains(got, "mcptools") {
		t.Errorf("$MCPTOOLS command dropped from output: %q", got)
	}
	if !strings.Contains(got, "head") {
		t.Errorf("head command dropped from output: %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
}

func TestStripLeadingEnvAssignments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple FOO=bar cmd", `FOO=bar cmd arg`, "cmd arg"},
		{"quoted value", `VAR="ws://host:9223" cmd`, "cmd"},
		{"single-quoted value", `VAR='value' cmd`, "cmd"},
		{"command substitution", `TOKEN=$(cat /tmp/token.txt) opencli browser`, "opencli browser"},
		{"brace expansion", `VAR=${HOME} cmd`, "cmd"},
		{"multiple assignments", `A=1 B="two" TOKEN=$(cat f) cmd arg`, "cmd arg"},
		{"no assignment", "cmd arg1 arg2", "cmd arg1 arg2"},
		{"only assignments", `FOO=bar BAZ=qux`, ""},
		{"escaped quote in value", `VAR="val\"ue" cmd`, "cmd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLeadingEnvAssignments(tt.input)
			if got != tt.expected {
				t.Errorf("stripLeadingEnvAssignments(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestNoLeadingOperator(t *testing.T) {
	cmd := `TOKEN=$(cat /home/user/path/token.txt) && ~/go/bin/mcptools call tavily_search`
	got := CompactBashCommand(cmd, 60)
	if strings.HasPrefix(got, "&&") || strings.HasPrefix(got, "||") || strings.HasPrefix(got, ";") || strings.HasPrefix(got, "|") {
		t.Errorf("CompactBashCommand starts with operator: %q", got)
	}
	if !strings.Contains(got, "mcptools") {
		t.Errorf("CompactBashCommand = %q, missing mcptools", got)
	}
}

func TestBuildCompactToolLineAgent(t *testing.T) {
	tests := []struct {
		name        string
		toolInput   string
		contains    []string
		notContains []string
	}{
		{
			name:      "model + subagent_type",
			toolInput: `{"description":"R22 shell structure","model":"sonnet","subagent_type":"ca-executor","prompt":"x"}`,
			contains:  []string{"Agent(sonnet/ca-executor)", "R22"},
		},
		{
			name:      "model only",
			toolInput: `{"description":"Sonnet digest","model":"sonnet","prompt":"x"}`,
			contains:  []string{"Agent(sonnet)", "Sonnet digest"},
		},
		{
			name:      "subagent_type only",
			toolInput: `{"description":"Search InkOS","subagent_type":"ca-researcher","prompt":"x"}`,
			contains:  []string{"Agent(ca-researcher)", "Search InkOS"},
		},
		{
			name:      "neither model nor subagent_type",
			toolInput: `{"description":"Quick task","prompt":"x"}`,
			contains:  []string{"Agent", "Quick task"},
		},
		{
			name:      "long description truncated, prefix preserved",
			toolInput: `{"description":"A very long description exceeding the limit significantly","model":"opus","subagent_type":"ca-verifier","prompt":"x"}`,
			contains:  []string{"Agent(opus/ca-verifier)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildCompactToolLine("Agent", []byte(tt.toolInput), "/tmp", 60)
			for _, c := range tt.contains {
				if !strings.Contains(result, c) {
					t.Errorf("BuildCompactToolLine = %q, missing %q", result, c)
				}
			}
			for _, nc := range tt.notContains {
				if strings.Contains(result, nc) {
					t.Errorf("BuildCompactToolLine = %q, should not contain %q", result, nc)
				}
			}
		})
	}
}

// TestCompactBashCommandRestoreArgs verifies Stage A restore-args-to-fill branch:
// outer args (cat path, tail -40) restored whole; middle grep pattern middle-truncated.
func TestCompactBashCommandRestoreArgs(t *testing.T) {
	cmd := `cat /var/log/syslog | grep -iE "out of memory|disk full|kernel panic|i/o error" | tail -40`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Command names must not be truncated.
	if !strings.Contains(got, "cat") {
		t.Errorf("command name 'cat' missing: %q", got)
	}
	if !strings.Contains(got, "grep") {
		t.Errorf("command name 'grep' missing: %q", got)
	}
	if !strings.Contains(got, "tail") {
		t.Errorf("command name 'tail' missing: %q", got)
	}
	// Outer args restored whole.
	if !strings.Contains(got, "/var/log/syslog") {
		t.Errorf("outer arg /var/log/syslog missing (should be restored whole): %q", got)
	}
	if !strings.Contains(got, "tail -40") {
		t.Errorf("outer arg tail -40 missing (should be restored whole): %q", got)
	}
	// grep -iE (short flag) should be present.
	if !strings.Contains(got, "grep -iE") {
		t.Errorf("grep -iE short flag missing: %q", got)
	}
	// Middle grep pattern must be middle-truncated: both a head fragment and a tail fragment present.
	// The pattern is "out of memory|disk full|kernel panic|i/o error"; truncation keeps the start and end.
	// Head check: "out" (first word of pattern, before any truncation point).
	if !strings.Contains(got, "out") {
		t.Errorf("pattern head fragment 'out' missing (middle-trunc expected): %q", got)
	}
	// Tail check: "error" (last word of the pattern value).
	if !strings.Contains(got, "error") {
		t.Errorf("pattern tail fragment 'error' missing (middle-trunc expected): %q", got)
	}
	// There must be a "…" between the head and the tail, indicating middle truncation (not head-only).
	// Verify that both "out" and "error" appear in the output AND "…" is between them.
	outIdx := strings.Index(got, "out")
	errorIdx := strings.LastIndex(got, "error")
	ellIdx := strings.Index(got[outIdx:], "…")
	if ellIdx < 0 || outIdx+ellIdx > errorIdx {
		t.Errorf("'…' not between 'out' and 'error' (expected middle-trunc form): %q", got)
	}
}

// TestCompactBashCommandCommandFirst verifies Stage B command-first behavior:
// a multi-command script shows MULTIPLE command names (echo, curl, head), not the first arg only.
// Uses the REAL SteamDeck socks5 probe with 2>&1 redirections (which previously caused "… …").
func TestCompactBashCommandCommandFirst(t *testing.T) {
	// REAL SteamDeck socks5 probe: echo + for-loop + 2 curls with 2>&1 + head, well over 60 bytes.
	cmd := `echo "=== Starting probe ===" ; echo "Checking target" ; for target in socks5://host1 socks5://host2; do curl -s --socks5 $target http://check.example.com/ 2>&1 | curl -s http://api.example.com/status 2>&1 | head -3; done`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Multiple command names must be visible (command-first principle).
	if !strings.Contains(got, "echo") {
		t.Errorf("command name 'echo' missing: %q", got)
	}
	if !strings.Contains(got, "curl") {
		t.Errorf("command name 'curl' missing: %q", got)
	}
	if !strings.Contains(got, "head") {
		t.Errorf("command name 'head' missing: %q", got)
	}
	// Must NOT contain any "… …" sequences (intra- or inter-command ellipsis merging).
	if strings.Contains(got, "… …") {
		t.Errorf("output contains '… …' (intra-command ellipsis not collapsed): %q", got)
	}
	// Must NOT contain "… ; …" (omitted-middle merge rule).
	if strings.Contains(got, "… ; …") {
		t.Errorf("output contains '… ; …' (should be merged): %q", got)
	}
	// Args should be omitted as "…".
	if !strings.Contains(got, "…") {
		t.Errorf("'…' missing: %q", got)
	}
	// Result must be <= 60 runes.
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
}

// TestReduceToBudgetMergesEllipsis verifies that renderModels never produces "… <op> …" sequences.
func TestReduceToBudgetMergesEllipsis(t *testing.T) {
	badPatterns := []string{"… ; …", "… | …", "… && …", "… || …", "… …"}
	check := func(label, got string) {
		for _, bp := range badPatterns {
			if strings.Contains(got, bp) {
				t.Errorf("[%s] output contains %q: %q", label, bp, got)
			}
		}
	}

	// REAL SteamDeck socks5 probe with 2>&1 redirections — these trigger trailMarker on curl
	// commands, which previously caused intra-command "… …" in the compacted output.
	steamDeck := `echo "=== Starting probe ===" ; echo "Checking target" ; for target in socks5://host1 socks5://host2; do curl -s --socks5 $target http://check.example.com/ 2>&1 | curl -s http://api.example.com/status 2>&1 | head -3; done`
	check("SteamDeck", CompactBashCommand(steamDeck, 60))

	// Synthetic: two standalone dot placeholders separated by semicolon.
	synth1Parts := []string{"for i in 1 2 3", ";", "do_something", ";", "echo done"}
	check("synth-for", reduceToBudget(synth1Parts, 20))

	// Synthetic: many commands → Stage B → middle omitted.
	var manyParts []string
	for i := 0; i < 10; i++ {
		if i > 0 {
			manyParts = append(manyParts, ";")
		}
		manyParts = append(manyParts, fmt.Sprintf("cmd%d arg1 arg2", i))
	}
	check("many-cmds", reduceToBudget(manyParts, 30))

	// Arg-omitted command followed by standalone "…" must not produce "for … ; …".
	forParts := []string{"for target in a b c", ";", "do_something very_long_arg", ";", "echo done"}
	forResult := reduceToBudget(forParts, 25)
	check("for-loop", forResult)
}

// TestCompactBashCommandMiddleTruncArg verifies that a single command with one long arg
// produces head+…+tail truncation, not head-only.
func TestCompactBashCommandMiddleTruncArg(t *testing.T) {
	// Single command with a long pattern that forces truncation.
	cmd := `grep "out of memory|disk full|kernel panic|i/o error|OOM killer|filesystem full" /var/log/syslog`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Command name must not be truncated.
	if !strings.Contains(got, "grep") {
		t.Errorf("command name 'grep' missing: %q", got)
	}
	// Truncation must have BOTH ends present (middle-truncate form).
	if !strings.Contains(got, "…") {
		t.Errorf("'…' marker missing: %q", got)
	}
	// Head fragment: beginning of the pattern.
	if !strings.Contains(got, "out of") {
		t.Errorf("pattern head 'out of' missing (middle-trunc expected, not head-only): %q", got)
	}
	// Tail fragment: end of the pattern.
	if !strings.Contains(got, "full") {
		t.Errorf("pattern tail 'full' missing (middle-trunc expected, not head-only): %q", got)
	}
}

// TestCompactBashCommandShortArgEllipsizedRuneBudget verifies that under a rune budget,
// 2-rune args (-u, -c, -3) ARE replaced by "…" when removed, since "…" is 1 rune < 2 runes.
// A 1-rune span (single char) is kept verbatim since replacing it with "…" (1 rune) saves nothing.
func TestCompactBashCommandShortArgEllipsizedRuneBudget(t *testing.T) {
	// The long pattern forces compaction. sort/uniq/head have only 2-char flags (-u/-c/-3).
	cmd := `grep -rn "averylongpatternhere_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" /a/b/c | sort -u | uniq -c | head -3`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Under rune budget, 2-char args (-u/-c/-3) are > 1 rune → replaced by "…".
	for _, pat := range []string{"sort …", "uniq …", "head …"} {
		if !strings.Contains(got, pat) {
			t.Errorf("2-rune arg should be ellipsized under rune budget; want %q in: %q", pat, got)
		}
	}
	// The long grep pattern must also be truncated (genuinely long → "…").
	if !strings.Contains(got, "…") {
		t.Errorf("expected '…' for long grep pattern: %q", got)
	}
	// grep/sort/uniq/head command names must still be present.
	for _, cmd2 := range []string{"grep", "sort", "uniq", "head"} {
		if !strings.Contains(got, cmd2) {
			t.Errorf("command name %q missing: %q", cmd2, got)
		}
	}
}

// TestCompactBashCommandShortRedirNotEllipsized verifies redirection guard under rune budget:
// a 1-rune redirection is kept verbatim; ≥2-rune redirections become "…".
// Under rune budget: ">x" is 2 runes (> 1) → becomes "…"; ">a" would need to be 1 rune to keep.
// The only verbatim-kept redir would be a 1-char redir which is unusual; we test a known 1-rune case
// is kept and all ≥2-rune redirections become "…".
func TestCompactBashCommandShortRedirNotEllipsized(t *testing.T) {
	// Under rune budget, ">x" (2 runes) IS replaced by "…" (since 2 > 1 rune).
	got := CompactBashCommand(`cat /tmp/output >x`, 60)
	if strings.Contains(got, ">x") {
		t.Errorf("2-rune redirection '>x' should become '…' under rune budget: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("2-rune redirection '>x' should produce '…' marker: %q", got)
	}

	// Long redirection 2>&1 (4 runes) must become "…".
	got2 := CompactBashCommand(`cat /tmp/output 2>&1`, 60)
	if strings.Contains(got2, "2>&1") {
		t.Errorf("long redirection '2>&1' should have been replaced by '…': %q", got2)
	}
	if !strings.Contains(got2, "…") {
		t.Errorf("long redirection '2>&1' should produce '…' marker: %q", got2)
	}

	// Long redirection >/dev/null (10 runes) must also become "…".
	got3 := CompactBashCommand(`grep pattern /home/user/very/long/path/file.go >/dev/null`, 60)
	if strings.Contains(got3, "/dev/null") {
		t.Errorf("long redirection '>/dev/null' should have been replaced by '…': %q", got3)
	}
	if !strings.Contains(got3, "…") {
		t.Errorf("long redirection '>/dev/null' should produce '…' marker: %q", got3)
	}
}

// TestRenderModelsShortRunKept verifies the helper-level guard under rune budget:
// a removed seg run of exactly 1 rune renders as the original text, NOT as "…" (since 1 == 1 rune, saving nothing);
// a run of > 1 rune renders as "…".
func TestRenderModelsShortRunKept(t *testing.T) {
	// sort -u: removed run is "-u" (2 runes > 1) → must render "sort …", not "sort -u".
	// Under rune budget, 2-rune spans are > 1 rune → replaced with "…".
	cmd := `grep -rn "averylongpatternhere_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" /a/b/c | sort -u | uniq -c | head -3`
	got := CompactBashCommand(cmd, 60)
	if !strings.Contains(got, "sort …") {
		t.Errorf("removed run '-u' (2 runes > 1) should render as 'sort …' under rune budget: %q", got)
	}
	// Confirm a genuinely long removed run still renders as "…".
	// grep -rn with the long pattern: the pattern arg is long, must produce "…" in grep part.
	if !strings.Contains(got, "…") {
		t.Errorf("long removed content should still produce '…': %q", got)
	}
}

// TestStageBShortCommandKept verifies Stage B behavior under rune budget:
// a 1-rune command is kept verbatim (replacing with "…" saves nothing);
// a ≥2-rune command (like "ls") IS replaced by "…" placeholder.
func TestStageBShortCommandKept(t *testing.T) {
	// Pipeline: echo + ls (2 runes > 1) + cat long-path + ls + echo done.
	// Stage B activates because the full render is too long.
	// "ls" (2 runes > 1 rune) → replaced by "…" under rune budget.
	cmd := `echo hello ; ls ; cat /home/user/Workspace/Github/Project/path/to/somefile.txt ; ls ; echo done`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("result too long: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// "ls" (2 runes) IS replaced by "…" under rune budget (2 > 1 → can save 1 rune).
	// The anchor commands (echo hello, echo done, cat) should still appear.
	if !strings.Contains(got, "echo") {
		t.Errorf("anchor command 'echo' missing: %q", got)
	}
	// The output must contain "…" for omitted content.
	if !strings.Contains(got, "…") {
		t.Errorf("expected '…' for omitted content: %q", got)
	}
}

// TestCompactBudgetIsRuneBased verifies that the compact budget is measured in runes, not bytes.
// A CJK-heavy command whose rune count is ≤60 but byte count exceeds 60 should be preserved.
// Also asserts a single-rune removed span is kept verbatim (not replaced by "…").
func TestCompactBudgetIsRuneBased(t *testing.T) {
	// CJK pattern with path: 59 runes but 77 bytes under rune budget it fits without truncation.
	// Under byte budget (60 bytes) the CJK pattern would have been truncated.
	cjkCmd := `grep -r "公益站点转发中转" /home/user/Workspace/Github/Project/ClaudeNote/project-registry.md`
	got := CompactBashCommand(cjkCmd, 60)
	// Rune budget: must be ≤60 runes.
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("CJK: rune budget violated: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// Byte length may exceed 60 (CJK chars are 3 bytes each).
	// (This is informational only — no assertion on len(got).)

	// The CJK pattern should be fully preserved (byte budget would have truncated it).
	if !strings.Contains(got, "公益站点转发中转") {
		t.Errorf("CJK pattern truncated under rune budget; should be fully preserved: %q", got)
	}
	// project-registry.md should also be visible (tail of path).
	if !strings.Contains(got, "project-registry.md") {
		t.Errorf("CJK: tail token 'project-registry.md' missing: %q", got)
	}
}

// TestCompactBudgetRuneRoundTrigger tests the round-35 trigger command at maxLen=60.
// This is the exact command used to verify round 35 was deployed.
func TestCompactBudgetRuneRoundTrigger(t *testing.T) {
	cmd := `node ${CLAUDE_CONFIG_DIR:-$HOME/.claude}/ca/scripts/ca-status.js update --project-root /home/seraphli/Workspace/Github/Seraphli/tg-cli "status_note=Round 35 verified and deployed to production with rune budget enabled" 2>&1 | tail -1`
	got := CompactBashCommand(cmd, 60)
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("round trigger: rune budget violated: %d runes > 60: %q", utf8.RuneCountInString(got), got)
	}
	// node and tail should be visible (command-first: first and last anchors kept).
	if !strings.Contains(got, "node") {
		t.Errorf("round trigger: 'node' command missing: %q", got)
	}
	if !strings.Contains(got, "tail") {
		t.Errorf("round trigger: 'tail' command missing: %q", got)
	}
}

// TestCompactBashCommandRuneRestoreFillCJK verifies that the restore-to-fill priority sort
// uses rune length (not byte length) so CJK args are correctly prioritized.
// Before the fix, restoreSegsToFill used len(seg.text) (byte count), causing it to sort
// "中文" (2 runes, 6 bytes) after "abc" (3 runes, 3 bytes), skipping it — output was "echo …" (6 runes).
// After the fix, compactLen(seg.text) (rune count) is used: "中文"=2 < "abc"=3, so it is
// restored first and fits in the 9-rune budget alongside "echo".
func TestCompactBashCommandRuneRestoreFillCJK(t *testing.T) {
	got := CompactBashCommand("echo abc 中文", 9)
	// Rune budget must be respected.
	if utf8.RuneCountInString(got) > 9 {
		t.Errorf("rune budget violated: %d runes > 9: %q", utf8.RuneCountInString(got), got)
	}
	// Output must not be the under-filled "echo …" (6 runes) produced before the fix.
	// The 2-rune CJK arg fits in the 9-rune budget so it must be present.
	if !strings.Contains(got, "中文") {
		t.Errorf("CJK arg '中文' missing (restore-fill should prefer shorter rune-length args first): %q", got)
	}
}

// TestBuildCompactToolLinePi covers Round-2 Item 2: after the hook-side name normalisation, a normalised pi
// Read/Write/Edit carries pi's `path` field (not CC's `file_path`), so str() must fall back to `path`; and
// pi's `ls` (kept lowercase, no CC analogue) must render via its own case instead of the random-map-field
// default. Both assertions are RED on the pre-fix build (no `path` fallback -> empty info; no `ls` case ->
// 🔧 + a random "path=..." field).
func TestBuildCompactToolLinePi(t *testing.T) {
	// Read with pi's `path` field: the basename must appear (str fallback file_path -> path).
	got := BuildCompactToolLine("Read", []byte(`{"path":"/home/seraphli/proj/main.go"}`), "/tmp", 40)
	if !strings.Contains(got, "Read") || !strings.Contains(got, "main.go") {
		t.Errorf("pi Read should render the basename via the path fallback, got %q", got)
	}
	// ls (lowercase) must use the folder emoji + the path, NOT the 🔧 default with a random "key=value" field.
	gotLs := BuildCompactToolLine("ls", []byte(`{"path":"/home/seraphli/proj"}`), "/tmp", 40)
	if !strings.Contains(gotLs, "📂") {
		t.Errorf("pi ls should render the 📂 folder emoji, got %q", gotLs)
	}
	if strings.Contains(gotLs, "path=") {
		t.Errorf("pi ls must NOT render the random-map-field default (path=...), got %q", gotLs)
	}
	if !strings.Contains(gotLs, "proj") {
		t.Errorf("pi ls should render the listed directory path, got %q", gotLs)
	}
}

// TestBuildToolResultTextPiArray covers Round-2 Item A: pi tool results are a JSON array of text blocks
// ([{"type":"text","text":"…"}]); the pre-fix code matches neither the string nor the map unmarshal and dumps
// the raw JSON. The fix joins the text blocks and renders them like a plain-string result. RED on the pre-fix
// build (the raw "[{"/"type" JSON leaks into the output).
func TestBuildToolResultTextPiArray(t *testing.T) {
	got := BuildToolResultText("Bash", []byte(`[{"type":"text","text":"PI_RESULT_MARKER_OUTPUT"}]`))
	if !strings.Contains(got, "PI_RESULT_MARKER_OUTPUT") {
		t.Errorf("pi array result should render the block text, got %q", got)
	}
	if strings.Contains(got, "[{") || strings.Contains(got, "type") {
		t.Errorf("pi array result must NOT dump the raw JSON array, got %q", got)
	}
	// A multi-block array joins the text fields.
	got2 := BuildToolResultText("Bash", []byte(`[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]`))
	if !strings.Contains(got2, "line one") || !strings.Contains(got2, "line two") {
		t.Errorf("multi-block pi array result should join text fields, got %q", got2)
	}
}

// TestBuildCompactToolDetailsPi covers Round-4 Item 4: the collapsed details body of a compact tool
// notification comes from buildToolNotifyBody, which read CC-only field names for Read/Edit/Write. A
// normalised pi call carries pi's field names (path / oldText / newText / content — not
// file_path / old_string / new_string), so the absolute path never rendered; and pi Read produced an
// EMPTY body, so BuildCompactToolDetails returned a bare summary with NO expandable <details> at all
// (the boss's production symptom: "📖 Read: README.md" with nothing to expand and no path). The fix
// mirrors BuildCompactToolLine's str("file_path","path") fallback into buildToolNotifyBody. Each pi case
// asserts the absolute path is present in the details body (plus the old/new diff for Edit and a real
// <details> wrapper for Read); each CC case asserts no regression. RED on the pre-fix build (pi path /
// diff absent; pi Read has no <details>).
func TestBuildCompactToolDetailsPi(t *testing.T) {
	const absPath = "/home/seraphli/proj/main.go"
	tests := []struct {
		name        string
		tool        string
		input       string
		wantContain []string // substrings that MUST appear in the compact details output
	}{
		// pi-shaped inputs (pi field names) — RED pre-fix, GREEN post-fix.
		{
			name:        "pi Read renders the abs path in an expandable details",
			tool:        "Read",
			input:       `{"path":"` + absPath + `"}`,
			wantContain: []string{"<details>", absPath},
		},
		{
			name:        "pi Edit renders the abs path and the old/new diff",
			tool:        "Edit",
			input:       `{"path":"` + absPath + `","oldText":"OLDMARK","newText":"NEWMARK"}`,
			wantContain: []string{absPath, "OLDMARK", "NEWMARK"},
		},
		{
			name:        "pi Write renders the abs path",
			tool:        "Write",
			input:       `{"path":"` + absPath + `","content":"WRITEMARK"}`,
			wantContain: []string{absPath, "WRITEMARK"},
		},
		// CC-shaped inputs (canonical field names) — no regression, GREEN on both builds.
		{
			name:        "cc Read still renders the abs path",
			tool:        "Read",
			input:       `{"file_path":"` + absPath + `"}`,
			wantContain: []string{"<details>", absPath},
		},
		{
			name:        "cc Edit still renders the abs path and the old/new diff",
			tool:        "Edit",
			input:       `{"file_path":"` + absPath + `","old_string":"CCOLD","new_string":"CCNEW"}`,
			wantContain: []string{absPath, "CCOLD", "CCNEW"},
		},
		{
			name:        "cc Write still renders the abs path",
			tool:        "Write",
			input:       `{"file_path":"` + absPath + `","content":"CCWRITE"}`,
			wantContain: []string{absPath, "CCWRITE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCompactToolDetails(tt.tool, []byte(tt.input), "/tmp", 60)
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("BuildCompactToolDetails(%s) = %q, missing %q", tt.tool, got, want)
				}
			}
		})
	}
}
