//go:build !darwin

package main

import (
	"errors"
	"syscall"
)

func bindProbeInterface(string) func(string, string, syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error { return errors.New("macOS local observer requires Darwin") }
}
