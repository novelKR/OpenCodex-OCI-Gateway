//go:build !darwin

package release

import "context"

type systemAppBundleValidator struct{}

func (systemAppBundleValidator) Validate(
	context.Context,
	string,
	string,
	Artifact,
	int,
	[]byte,
	string,
) (BundleValidation, error) {
	return BundleValidation{}, ErrStageInvalidBundle
}
