package contract_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGoAndTypeScriptHighLevelSDKBehaviorParity(t *testing.T) {
	root := repositoryRoot(t)
	goSurface := readSurfaceFiles(t, root, []string{
		"sdk/go/secondboxclient/sdk.go", "sdk/go/secondboxclient/lifecycle.go",
		"sdk/go/secondboxclient/resources.go", "sdk/go/secondboxclient/data_plane.go",
		"sdk/go/secondboxclient/exec_stream.go", "sdk/go/secondboxclient/terminal.go",
	})
	typeScriptSurface := readSurfaceFiles(t, root, []string{"sdk/typescript/client.ts"})
	operations := []struct{ goName, typeScriptName string }{
		{"ValidateProfile", "validateProfile"}, {"CreateSandbox", "createSandbox"},
		{"AdoptSandbox", "adoptSandbox"}, {"ListSandboxes", "listSandboxes"},
		{"UpdateMetadata", "updateMetadata"}, {"WaitFor", "waitFor"},
		{"Execute", "exec"}, {"CreateExecStream", "createExecStream"},
		{"ReadFile", "readFile"}, {"WriteFile", "writeFile"},
		{"StatFile", "statFile"}, {"ListDirectory", "listDirectory"},
		{"FileExists", "fileExists"}, {"CreateDirectory", "createDirectory"},
		{"RemovePath", "removePath"}, {"CreateSnapshot", "createSnapshot"},
		{"ListSnapshots", "listSnapshots"}, {"GetSnapshot", "getSnapshot"},
		{"DeleteSnapshot", "deleteSnapshot"}, {"UploadArtifact", "uploadArtifact"},
		{"ListArtifacts", "listArtifacts"}, {"GetArtifact", "getArtifact"},
		{"DownloadArtifact", "downloadArtifact"}, {"DeleteArtifact", "deleteArtifact"},
		{"TakeoverLease", "takeoverLease"}, {"CreatePortSession", "createPortSession"},
		{"GetPortSession", "getPortSession"}, {"ClosePortSession", "closePortSession"},
		{"ConnectPortTunnel", "connectPortTunnel"}, {"CreateTerminal", "createTerminal"},
		{"GetTerminal", "getTerminal"}, {"CancelTerminal", "cancelTerminal"},
		{"ConnectTerminal", "connectTerminal"},
	}
	for _, operation := range operations {
		if !strings.Contains(goSurface, " "+operation.goName+"(") {
			t.Errorf("Go SDK lacks high-level %s", operation.goName)
		}
		if !strings.Contains(typeScriptSurface, " "+operation.typeScriptName+"(") {
			t.Errorf("TypeScript SDK lacks high-level %s", operation.typeScriptName)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate SDK parity test source")
	}
	return filepath.Join(filepath.Dir(source), "..", "..")
}

func readSurfaceFiles(t *testing.T, root string, names []string) string {
	t.Helper()
	var source strings.Builder
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		source.Write(content)
	}
	return source.String()
}
