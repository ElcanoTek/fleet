//go:build !linux

package admincli

import "os/exec"

// waitForeground is a plain Wait off Linux (no waitid peek): the prompts still
// read the terminal; only Ctrl-Z suspension of the wrapper is Linux-only.
func waitForeground(cmd *exec.Cmd, _ int) error {
	return cmd.Wait()
}
