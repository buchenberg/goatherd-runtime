//go:build !linux

package croft

// Sandbox is the non-Linux placeholder for the containerd backend.
//
// It exists so the package's exported surface is the same shape on every
// platform — a host can name the type, build against it, and carry containerd
// settings in one config struct — while New always refuses. It deliberately
// implements nothing: there is no containerd substrate here to delegate to,
// and a type that satisfied sbx.Sandbox by returning errors from every method
// would be a backend that looks openable and is not.
//
// See the package doc for why the implementation is Linux-only.
type Sandbox struct {
	cfg Config
}

// New always returns ErrUnsupportedPlatform on this build.
//
// No backend is registered in init on this platform, so the registry path
// fails first and more informatively: OpenSandbox("containerd", ...) returns
// the unknown-backend error listing what *is* available. New is refused too,
// so a host that constructs the backend directly rather than through the
// registry gets the same answer instead of a value it cannot use.
func New(cfg Config) (*Sandbox, error) {
	return nil, ErrUnsupportedPlatform
}

// Backend reports the backend identifier, so diagnostics and config validation
// can name the backend a caller asked for even where it cannot be provided.
func (s *Sandbox) Backend() string { return BackendName }
