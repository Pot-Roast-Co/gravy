package host

import (
	"strings"
	"testing"
)

// TestRemoteCommandQuotesTheShell is a real failure on a Windows host reached over ssh.
//
// Git Bash reports $SHELL as "/c/program files/git/bin/bash.exe". Unquoted, the remote shell
// split it on the space and every command on that machine failed with "/c/program: No such file
// or directory" — which reads like a broken host rather than a quoting bug here.
func TestRemoteCommandQuotesTheShell(t *testing.T) {
	got := remoteCommand(ExecSpec{Cmd: "git", Args: []string{"--version"}})
	if !strings.HasPrefix(got, `exec "$SHELL" -lic `) {
		t.Errorf("remoteCommand() = %q, want it to quote $SHELL", got)
	}
}

// TestRemoteCommandCarriesEnvAndDir: both are rendered before the command, so a run lands in the
// worktree with its environment set.
//
// The whole payload is one quoted argument to the login shell, so every quote inside it is
// escaped. Asserting the rendered line rather than its parts is what catches a change that nests
// the quoting wrongly.
func TestRemoteCommandCarriesEnvAndDir(t *testing.T) {
	got := remoteCommand(ExecSpec{
		Cmd: "go", Args: []string{"test", "./..."},
		Dir: "/work/tree", Env: map[string]string{"CI": "1"},
	})
	want := `exec "$SHELL" -lic 'export CI='\''1'\''; cd '\''/work/tree'\'' && '\''go'\'' '\''test'\'' '\''./...'\'''`
	if got != want {
		t.Errorf("remoteCommand()\n got = %s\nwant = %s", got, want)
	}
}

// TestShellQuoteHandlesQuotes: a path with a single quote in it must not end the quoting.
func TestShellQuoteHandlesQuotes(t *testing.T) {
	if got, want := shellQuote("bobby's mac"), `'bobby'\''s mac'`; got != want {
		t.Errorf("shellQuote() = %q, want %q", got, want)
	}
}
