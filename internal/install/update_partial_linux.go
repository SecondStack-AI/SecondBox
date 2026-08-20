//go:build linux

package install

func validateUpdatePartialRemoval(path string) error {
	return validateNoNestedMounts(path)
}
