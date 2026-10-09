//go:build e2e

package e2e

import "syscall"

// serveProcAttr takes serve down with the test binary if it dies without running stop.
func serveProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
