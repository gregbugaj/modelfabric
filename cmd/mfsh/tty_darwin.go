package main

import "syscall"

// macOS uses BSD TIOCGETA/TIOCSETA values rather than Linux TCGETS/TCSETS.
const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
)
