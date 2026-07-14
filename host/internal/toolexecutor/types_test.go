package toolexecutor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOperationsMatchCrossPlaneContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "tool-executor-operations.json"))
	if err != nil {
		t.Fatalf("read operation contract: %v", err)
	}
	var want []Operation
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatalf("decode operation contract: %v", err)
	}
	if got := Operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}
