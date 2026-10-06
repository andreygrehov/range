//go:build !(darwin && cgo)

package session

// newVZ reports that this build has no Virtualization.framework runtime.
func newVZ() (Runtime, bool) { return nil, false }
