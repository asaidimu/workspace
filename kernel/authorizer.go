package kernel

import (
	"context"
)

// allowOwner is the authorizer for single-user apps: it permits every command
// whose principal has an owner. It is deliberately the thinnest possible
// check, because resource-level ownership is not this layer's job — the
// store's owner-scoped repositories are a second, independent wall
// (Workspace specification, section 7), and they reject a read or write
// whose owner does not match the scope. A principal with no owner has not
// been authenticated as anyone, so it is refused here.
type allowOwner struct{}

// AllowOwner is the one-liner authorizer for single-user apps. Opening a
// workspace without an authorizer is an error, and this is the sanctioned
// way to say "trust the owner scoping in the store".
func AllowOwner() Authorizer { return allowOwner{} }

// Authorize implements Authorizer.
func (allowOwner) Authorize(ctx context.Context, p Principal, commandName string, t Target) error {
	if p.Owner == "" {
		return NewError(CodeForbidden, "principal has no owner").
			WithOperation(commandName).
			WithPath(t.ID)
	}
	return nil
}
