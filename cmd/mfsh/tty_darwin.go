package main

import "syscall"

// macOS uses the BSD names; the numbers differ from Linux's TCGETS/TCSETS,
// which is why the shared constants were wrong here.
const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
)
