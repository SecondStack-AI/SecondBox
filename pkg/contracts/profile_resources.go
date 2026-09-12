package contracts

import (
	"encoding/json"
	"errors"
)

// UnmarshalJSON keeps an explicit null object from becoming an absent ceiling.
// Axis presence and bounds are validated when the revision is published.
func (ceiling *ProfileResourceCeiling) UnmarshalJSON(data []byte) error {
	type resourceCeiling ProfileResourceCeiling
	var value resourceCeiling
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == nil {
		return errors.New("SecondBox Profile resourceCeiling must be an object, not null")
	}
	*ceiling = ProfileResourceCeiling(value)
	return nil
}
