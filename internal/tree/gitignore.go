package tree

import (
	"os/exec"
	"strings"
)

func detectGitRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

func checkIgnored(repoDir string, paths []string) map[string]bool {
	result := map[string]bool{}
	if len(paths) == 0 {
		return result
	}
	cmd := exec.Command("git", "-C", repoDir, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		// exit 1 = nothing ignored; any other error -> best effort, skip
		return result
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			result[line] = true
		}
	}
	return result
}
