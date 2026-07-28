package runnercontrol

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestGeneratedRunnerProtocolMatchesFrozenCanonicalDescriptor(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate generated runner protocol conformance test")
	}
	descriptorPath := filepath.Join(
		filepath.Dir(sourceFile), "..", "..",
		"contracts", "runner", "v1", "runner.descriptor.pb",
	)
	descriptorBytes, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	var descriptorSet descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descriptorBytes, &descriptorSet); err != nil {
		t.Fatal(err)
	}
	if len(descriptorSet.File) != 1 {
		t.Fatalf("frozen runner descriptor file count = %d, want 1", len(descriptorSet.File))
	}
	generated := protodesc.ToFileDescriptorProto(runnerv1.File_contracts_runner_v1_runner_proto)
	if !proto.Equal(descriptorSet.File[0], generated) {
		t.Fatal("root generated runner protocol is stale relative to the frozen canonical descriptor")
	}
}
