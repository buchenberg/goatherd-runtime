//go:build !windows

package main

import "fmt"

// run reports that there is nothing to prove off Windows. The mechanism is a
// Windows primitive, so the spike has no counterpart on another platform.
func run() int {
	fmt.Println("winacl-spike: the Windows write-restriction mechanism exists only on Windows; nothing to prove here.")
	return 0
}
