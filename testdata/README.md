# Runtime testdata

These are copies of the kernel's cross-language golden vectors, kept here so the
runtime's test suites can replay them without reading across the module
boundary:

- `execution_vectors_v0.json` — the execution/relation identity derivations and
  record sequences, replayed by `task/` and the root package's tests.
- `materialize_vectors_v0.json` — the materialize request digests and
  declare → materialize sequences, replayed by `materialize/`.

The kernel (`github.com/buchenberg/shepherd-kernel-go`) is the source of truth:
`testkit/golden_provenance_test.go` pins their SHA-256 in the kernel module. If
the kernel regenerates a vector, copy the new file here in the same change.
