package ports

import "errors"

var ErrProfileIncompatible = errors.New("SecondBox target Profile is incompatible with the Sandbox")

// ProfileIncompatibleError names the pinned Sandbox property that the target
// Profile revision cannot serve.
type ProfileIncompatibleError struct {
	Property string
	Reason   string
}

func (err *ProfileIncompatibleError) Error() string {
	return ErrProfileIncompatible.Error() + ": " + err.Property + " " + err.Reason
}

func (err *ProfileIncompatibleError) Unwrap() error { return ErrProfileIncompatible }
