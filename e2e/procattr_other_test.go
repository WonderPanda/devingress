//go:build e2e && !linux

package e2e

import "syscall"

// serveProcAttr has no parent-death signal outside Linux; stop kills serve.
func serveProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
