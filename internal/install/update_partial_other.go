//go:build !linux

package install

func validateUpdatePartialRemoval(string) error {
	return nil
}
