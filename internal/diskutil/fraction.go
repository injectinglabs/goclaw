// Package diskutil exposes a thin filesystem-stat helper used by every
// component that needs to react to disk pressure (media cache sweeper,
// skills cache sweeper, future log/trace rotation). Single home means
// both Linux/Darwin and Windows stubs live in one place; component
// packages just call diskutil.Fraction(path) and trust the result.
package diskutil
