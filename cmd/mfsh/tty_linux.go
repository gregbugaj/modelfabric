package main

import "syscall"

// Linux reads and writes the terminal attributes with TCGETS/TCSETS.
const (
	ioctlReadTermios  = syscall.TCGETS
	ioctlWriteTermios = syscall.TCSETS
)
