package engine

import "errors"

// ErrInvalidResumeInput identifies a schema-invalid user continuation.
// It is emitted before the suspended leaf is advanced or any further effect
// is dispatched. Services may re-prompt using the same request ID without
// treating it as an ambiguous remotely attempted operation.
var ErrInvalidResumeInput = errors.New("invalid suspend continuation input")
