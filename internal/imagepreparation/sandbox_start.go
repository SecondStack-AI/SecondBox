package imagepreparation

import (
	"encoding/json"
	"errors"
)

func SandboxStartOperationID(metadata []byte, legacyEffectID string) (string, error) {
	var payload Payload
	if err := json.Unmarshal(metadata, &payload); err != nil {
		return "", err
	}
	if payload.Role == "" {
		return legacyEffectID, nil
	}
	if payload.Role != "sandbox-start" || payload.StartOperationID == "" {
		return "", errors.New("SecondBox Sandbox image preparation lacks its start Operation binding")
	}
	return payload.StartOperationID, nil
}
