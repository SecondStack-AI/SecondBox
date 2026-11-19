package contract_test

import (
	"encoding/json"
	"testing"
)

func TestRunnerFleetRoutesRequirePlatformAuthority(t *testing.T) {
	document := loadOpenAPIContract(t)
	paths := object(t, document["paths"], "paths")
	for path, method := range map[string]string{
		"/v1/runner-pools/{runnerPoolName}/fleet":                       "get",
		"/v1/runner-pools/{runnerPoolName}/runners/{runnerId}:evacuate": "post",
		"/v1/runner-pools/{runnerPoolName}/runners/{runnerId}:retire":   "post",
	} {
		item := object(t, paths[path], path)
		operation := object(t, item[method], path+" "+method)
		encoded, err := json.Marshal(operation["security"])
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != `[{"platformBearer":[]}]` {
			t.Fatalf("%s exposes non-platform authority: %s", path, encoded)
		}
	}
}
