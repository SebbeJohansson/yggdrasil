package sindri

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// UseShellPath puts the PATH your interactive shell sets up (nvm, pyenv,
// ~/.local/bin) in front of Sindri's own, so agents and checks find the
// same tools you do. A systemd unit starts with a bare PATH otherwise.
func UseShellPath(logf func(string, ...any)) {
	shells := []string{os.Getenv("SHELL"), "zsh", "bash"}
	for _, shell := range shells {
		if found := shellPath(shell); found != "" {
			merged := []string{}
			for _, dir := range append(strings.Split(found, ":"), strings.Split(os.Getenv("PATH"), ":")...) {
				if dir != "" && !slices.Contains(merged, dir) {
					merged = append(merged, dir)
				}
			}
			_ = os.Setenv("PATH", strings.Join(merged, ":"))
			return
		}
	}
	logf("could not read PATH from an interactive shell; keeping %s", os.Getenv("PATH"))
}

// shellPath is the PATH an interactive shell sets up, or "" when it gives
// none, as when a .bashrc ends by starting another shell.
func shellPath(shell string) string {
	if shell == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Markers keep whatever the shell's rc files print out of the PATH.
	out, _ := exec.CommandContext(ctx, shell, "-ic", `printf '\n<<PATH>>%s<<END>>\n' "$PATH"`).Output()
	text := string(out)
	start, end := strings.Index(text, "<<PATH>>"), strings.Index(text, "<<END>>")
	if start < 0 || end < start {
		return ""
	}
	return text[start+len("<<PATH>>") : end]
}
