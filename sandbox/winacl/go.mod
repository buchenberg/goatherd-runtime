module github.com/buchenberg/shepherd-runtime-go/sandbox/winacl

go 1.21

require (
	github.com/buchenberg/shepherd-runtime-go v0.17.0
	golang.org/x/sys v0.19.0
)

// The nested module develops against the in-repo root runtime. The replace is
// dropped at release, where the module requires a tagged root runtime instead
// (the sandbox/landlock release discipline).
replace github.com/buchenberg/shepherd-runtime-go => ../..
