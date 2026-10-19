// Command winacl-spike proves the Windows write-restriction mechanism that a
// shepherd Windows containment backend would use.
//
// The mechanism — a write-restricted token, per-root write capability SIDs, a
// Low integrity level, a Low no-write-up mandatory label, and an ambient-delete
// deny — exists only on Windows, so the implementation lives in
// main_windows.go and this entry point reports that there is nothing to prove
// elsewhere. The split keeps the module buildable on every platform, which
// the CI's nested-module job requires.
//
// Usage: winacl-spike            (parent: sets up and asserts)
//
//	winacl-spike --child OP PATH  (child: op in {write,read,delete,mkdir})
package main

import (
	"os"
)

func main() {
	os.Exit(run())
}
