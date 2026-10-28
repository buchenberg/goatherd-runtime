// This file is deliberately in the external test package (effect_test), not the
// in-package one (effect).
//
// It asserts that *goatherd.Scope satisfies effect.FactAppender, which requires
// importing the runtime root. An in-package test file that imports the root
// would form effect(test) -> goatherd-runtime -> effect the moment the root
// imports this package — and the root importing effect is the whole point of
// keeping effect a leaf. Go rejects that with "import cycle not allowed in
// test", so the assertion has to live where it can see both sides.
//
// This was verified, not assumed: adding an effect import to scope.go with the
// assertion in-package fails the build with exactly that error.
package effect_test

import (
	"testing"

	goatherd "github.com/buchenberg/goatherd-runtime"
	"github.com/buchenberg/goatherd-runtime/effect"
)

func TestScopeSatisfiesFactAppender(t *testing.T) {
	// The assignment is the assertion: it stops compiling if Scope's AppendFact
	// or OwnerID signature drifts, which is how the seam stays honest without a
	// real SQLite store in the test.
	var appender effect.FactAppender = (*goatherd.Scope)(nil)

	// A typed nil satisfies an interface, so this comparison is false. That is
	// not a tautology being checked — it is the hazard Emit's doc records:
	// `appender == nil` cannot catch a nil *Scope, and calling a method on it
	// would panic on the nil receiver rather than return Emit's documented
	// error. Pinning it here means a future "improvement" to that nil check
	// cannot quietly assume the guard is stronger than it is.
	if appender == nil {
		t.Error("a nil *Scope held in a FactAppender compared equal to nil; the typed-nil hazard Emit documents no longer applies and its doc is now wrong")
	}
}
