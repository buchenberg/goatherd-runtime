//go:build !linux

package landlock

// On a non-Linux host confinement cannot be established, so every trampoline
// invocation fails closed. The backend is never available there anyway.
func runExecTrampoline([]string) int  { return ConfineFailedRC }
func runProbeTrampoline([]string) int { return ConfineFailedRC }
