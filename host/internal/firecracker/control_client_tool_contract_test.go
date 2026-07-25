package microvm

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestToolExecutorOperationContract(t *testing.T) {
	required := []ToolExecutorOperation{
		ToolOpExec,
		ToolOpReadFile,
		ToolOpReadFileBuffer,
		ToolOpWriteFile,
		ToolOpStat,
		ToolOpReaddir,
		ToolOpExists,
		ToolOpMkdir,
		ToolOpRm,
	}
	if len(ToolExecutorOperations) != len(required) {
		t.Fatalf("operation count = %d, want %d", len(ToolExecutorOperations), len(required))
	}
	for _, op := range required {
		if !slices.Contains(ToolExecutorOperations, op) {
			t.Fatalf("missing operation %q", op)
		}
	}
}

func TestToolExecutorRequestJSONShapes(t *testing.T) {
	tests := []struct {
		name string
		req  ToolExecRequest
		want string
	}{
		{
			name: "exec",
			req: ToolExecRequest{
				Operation:     ToolOpExec,
				Command:       "sh",
				Args:          []string{"-c", "pwd"},
				Cwd:           ".",
				Env:           map[string]string{"A": "B"},
				TimeoutMillis: 1000,
			},
			want: `"operation":"exec"`,
		},
		{name: "read text", req: ToolExecRequest{Operation: ToolOpReadFile, Path: "notes.txt"}, want: `"operation":"readFile"`},
		{name: "read buffer", req: ToolExecRequest{Operation: ToolOpReadFileBuffer, Path: "image.png"}, want: `"operation":"readFileBuffer"`},
		{name: "write", req: ToolExecRequest{Operation: ToolOpWriteFile, Path: "notes.txt", Content: "hello"}, want: `"operation":"writeFile"`},
		{name: "stat", req: ToolExecRequest{Operation: ToolOpStat, Path: "notes.txt"}, want: `"operation":"stat"`},
		{name: "readdir", req: ToolExecRequest{Operation: ToolOpReaddir, Path: "."}, want: `"operation":"readdir"`},
		{name: "exists", req: ToolExecRequest{Operation: ToolOpExists, Path: "notes.txt"}, want: `"operation":"exists"`},
		{name: "mkdir", req: ToolExecRequest{Operation: ToolOpMkdir, Path: "dir", Recursive: true}, want: `"operation":"mkdir"`},
		{name: "rm", req: ToolExecRequest{Operation: ToolOpRm, Path: "dir", Recursive: true, Force: true}, want: `"operation":"rm"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.req)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			if !strings.Contains(string(data), tt.want) {
				t.Fatalf("request JSON %s does not contain %s", data, tt.want)
			}
		})
	}
}

func TestToolExecutorPathContractRejectsEscapes(t *testing.T) {
	rejected := []string{
		"../secret",
		"/etc/passwd",
		"dir/../../secret",
		"ok\x00bad",
	}
	for _, path := range rejected {
		if toolExecutorPathContractAllows(path) {
			t.Fatalf("path %q should be rejected by guest contract", path)
		}
	}
	allowed := []string{"", ".", "/", "notes.txt", "dir/../notes.txt", "dir/sub"}
	for _, path := range allowed {
		if !toolExecutorPathContractAllows(path) {
			t.Fatalf("path %q should be allowed by guest contract", path)
		}
	}
}

func toolExecutorPathContractAllows(p string) bool {
	if strings.ContainsRune(p, 0) {
		return false
	}
	if strings.HasPrefix(p, "/") && p != "/" {
		return false
	}
	depth := 0
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			depth--
		default:
			depth++
		}
		if depth < 0 {
			return false
		}
	}
	return true
}
