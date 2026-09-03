//go:build !darwin

package containerruntime

import "context"

type APFSCloner struct{}

func NewAPFSCloner() *APFSCloner { return &APFSCloner{} }

func (*APFSCloner) Clone(context.Context, string, string) error { return ErrUnavailable }
