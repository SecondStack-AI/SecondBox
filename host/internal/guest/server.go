package microvmguest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"agentcy/internal/registry"
	"agentcy/internal/runtimecontext"
	"agentcy/internal/sandboxlimits"
	"agentcy/internal/toolexecutor"
)

const maxCommandOutputStreamBytes = 256 << 10

type commandOutputBuffer struct {
	value     []byte
	limit     int
	truncated bool
}

func newCommandOutputBuffer(limit int) *commandOutputBuffer {
	return &commandOutputBuffer{limit: limit}
}

func (b *commandOutputBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - len(b.value)
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		b.value = append(b.value, value[:remaining]...)
	}
	if remaining < len(value) {
		b.truncated = true
	}
	return written, nil
}

func (b *commandOutputBuffer) String() string {
	if !b.truncated {
		return string(b.value)
	}
	const marker = "\n[output truncated by tool executor]"
	if b.limit <= len(marker) {
		return marker[:b.limit]
	}
	prefix := b.value
	if len(prefix)+len(marker) > b.limit {
		prefix = prefix[:b.limit-len(marker)]
	}
	return string(prefix) + marker
}

const defaultFileTransferMaxBytes int64 = sandboxlimits.FileTransferMaxBytes

type Server struct {
	WorkspaceDir      string
	RuntimePrivateDir string
	InstanceID        string
	AgentID           string
	LogPath           string
	Freezer           WorkspaceFreezer
	Hardener          RestoreHardener
	Now               func() time.Time
}

type WorkspaceFreezer interface {
	Freeze(ctx context.Context, workspaceDir string) error
	Thaw(ctx context.Context, workspaceDir string) error
}

type RestoreHardener interface {
	Harden(ctx context.Context, input RestoreHardenInput) error
}

type SecretBundle struct {
	Env                      map[string]string         `json:"env,omitempty"`
	Files                    map[string]string         `json:"files,omitempty"`
	RuntimeContextProjection runtimecontext.Projection `json:"runtimeContextProjection,omitempty"`
}

type RestoreHardenRequest struct {
	HostTime      string `json:"hostTime,omitempty"`
	EntropyBase64 string `json:"entropyBase64,omitempty"`
}

const (
	toolOpExec           = toolexecutor.OpExec
	toolOpReadFile       = toolexecutor.OpReadFile
	toolOpReadFileBuffer = toolexecutor.OpReadFileBuffer
	toolOpWriteFile      = toolexecutor.OpWriteFile
	toolOpStat           = toolexecutor.OpStat
	toolOpReaddir        = toolexecutor.OpReaddir
	toolOpExists         = toolexecutor.OpExists
	toolOpMkdir          = toolexecutor.OpMkdir
	toolOpRm             = toolexecutor.OpRm
)

type toolExecRequest = toolexecutor.Request

var toolCommandRuntimeEnvAllowlist = map[string]bool{
	"AGENT_ID":                      true,
	"AGENTCY_COMPARTMENT_ID":        true,
	"AGENTCY_PROXY_EGRESS_ENABLED":  true,
	"AGENTCY_RUNTIME_CREDENTIAL_ID": true,
	"AGENTCY_RUNTIME_TOKEN":         true,
	"CURL_CA_BUNDLE":                true,
	"GIT_SSL_CAINFO":                true,
	"GIT_ASKPASS":                   true,
	"GIT_CONFIG_GLOBAL":             true,
	"GIT_TERMINAL_PROMPT":           true,
	"GH_TOKEN":                      true,
	"GITHUB_TOKEN":                  true,
	"HTTP_PROXY":                    true,
	"HTTPS_PROXY":                   true,
	"NODE_EXTRA_CA_CERTS":           true,
	"NO_PROXY":                      true,
	"PLATFORM_API_URL":              true,
	"REQUESTS_CA_BUNDLE":            true,
	"SSL_CERT_FILE":                 true,
	"TZ":                            true,
	"http_proxy":                    true,
	"https_proxy":                   true,
	"no_proxy":                      true,
}

type toolExecResponse = toolexecutor.Response
type toolExecutorDirEntry = toolexecutor.DirEntry

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/workspace/list", s.handleWorkspaceList)
	mux.HandleFunc("/workspace/read", s.handleWorkspaceRead)
	mux.HandleFunc("/workspace/write", s.handleWorkspaceWrite)
	mux.HandleFunc("/workspace/freeze", s.handleWorkspaceFreeze)
	mux.HandleFunc("/workspace/thaw", s.handleWorkspaceThaw)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/secrets/apply", s.handleSecretsApply)
	mux.HandleFunc("/restore/harden", s.handleRestoreHarden)
	mux.HandleFunc("/tool/exec", s.handleToolExec)
	return mux
}

func (s Server) handleHeartbeat(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instanceId": s.InstanceID,
		"agentId":    s.AgentID,
		"healthy":    true,
		"time":       now.Format(time.RFC3339Nano),
	})
}

func (s Server) handleWorkspaceList(w http.ResponseWriter, r *http.Request) {
	root, target, err := s.workspacePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	directory, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		if errors.Is(err, errUnsafeWorkspacePath) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, "workspace path not found")
		return
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	type entry struct {
		Path  string `json:"path"`
		Type  string `json:"type"`
		Size  int64  `json:"size,omitempty"`
		MTime string `json:"mtime,omitempty"`
	}
	out := struct {
		Entries []entry `json:"entries"`
	}{}
	for _, item := range entries {
		info, err := item.Info()
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, filepath.Join(target, item.Name()))
		if err != nil {
			continue
		}
		kind := "file"
		if info.IsDir() {
			kind = "dir"
		}
		out.Entries = append(out.Entries, entry{
			Path:  filepath.ToSlash(rel),
			Type:  kind,
			Size:  info.Size(),
			MTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s Server) handleWorkspaceRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	root, target, err := s.workspacePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, errUnsafeWorkspacePath) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, "workspace file not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "workspace path is not a regular file")
		return
	}
	maxBytes, err := fileTransferLimitFromQuery(r, defaultFileTransferMaxBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if info.Size() > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("workspace file size %d bytes exceeds file transfer limit of %d bytes", info.Size(), maxBytes))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, f)
}

func (s Server) handleWorkspaceWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	root, target, err := s.workspacePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	maxBytes, err := fileTransferLimitFromQuery(r, defaultFileTransferMaxBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parent, err := openWorkspaceParent(root, target, true)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnsafeWorkspacePath) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	defer parent.close()
	tmp, tmpName, err := createWorkspaceTemp(parent)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnsafeWorkspacePath) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			unlinkWorkspaceTemp(parent, tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	hash := sha256.New()
	counter := &countingLimitReader{r: r.Body, max: maxBytes}
	written, copyErr := io.Copy(tmp, io.TeeReader(counter, hash))
	if copyErr != nil {
		status := http.StatusInternalServerError
		message := copyErr.Error()
		if errors.Is(copyErr, errTransferTooLarge) {
			status = http.StatusRequestEntityTooLarge
			message = fmt.Sprintf("workspace write size %d bytes exceeds file transfer limit of %d bytes", counter.n, maxBytes)
		}
		writeError(w, status, message)
		return
	}
	if written != counter.n {
		writeError(w, http.StatusInternalServerError, "workspace write byte count mismatch")
		return
	}
	if err := tmp.Sync(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := commitWorkspaceTemp(parent, tmpName); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errUnsafeWorkspacePath) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	cleanup = false
	writeJSON(w, http.StatusOK, map[string]any{
		"size":   written,
		"sha256": fmt.Sprintf("%x", hash.Sum(nil)),
	})
}

func (s Server) handleWorkspaceFreeze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Freezer == nil {
		writeError(w, http.StatusServiceUnavailable, "workspace freezer unavailable")
		return
	}
	if err := s.Freezer.Freeze(r.Context(), s.workspaceRoot()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frozen": true})
}

func (s Server) handleWorkspaceThaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Freezer == nil {
		writeError(w, http.StatusServiceUnavailable, "workspace freezer unavailable")
		return
	}
	if err := s.Freezer.Thaw(r.Context(), s.workspaceRoot()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frozen": false})
}

func (s Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.LogPath) == "" {
		writeError(w, http.StatusNotFound, "log path not configured")
		return
	}
	f, err := os.Open(s.LogPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "log file not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain")
	data, err := tailLogFile(f, strings.TrimSpace(r.URL.Query().Get("tail")), sandboxlimits.ControlClientResponseBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = w.Write(data)
}

func tailLogFile(f *os.File, tail string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = sandboxlimits.ControlClientResponseBytes
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	buf := make([]byte, size-start)
	if len(buf) > 0 {
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	n, _ := strconv.Atoi(tail)
	if n <= 0 {
		return buf, nil
	}
	linesSeen := 0
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] != '\n' {
			continue
		}
		linesSeen++
		if linesSeen > n {
			return buf[i+1:], nil
		}
	}
	return buf, nil
}

func (s Server) handleSecretsApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var bundle SecretBundle
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&bundle); err != nil {
		writeError(w, http.StatusBadRequest, "invalid secret bundle")
		return
	}
	if err := s.applySecrets(bundle); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": true})
}

func (s Server) handleRestoreHarden(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req RestoreHardenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid restore harden request")
		return
	}
	hostTime := time.Now().UTC()
	if strings.TrimSpace(req.HostTime) != "" {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(req.HostTime))
		if err != nil {
			writeError(w, http.StatusBadRequest, "hostTime must be RFC3339Nano")
			return
		}
		hostTime = parsed.UTC()
	}
	hardener := s.Hardener
	if hardener == nil {
		hardener = LinuxRestoreHardener{}
	}
	entropy, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.EntropyBase64))
	if err != nil {
		writeError(w, http.StatusBadRequest, "entropyBase64 must be valid base64")
		return
	}
	if err := hardener.Harden(r.Context(), RestoreHardenInput{HostTime: hostTime, Entropy: entropy}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hardened": true, "hostTime": hostTime.Format(time.RFC3339Nano)})
}

func (s Server) handleToolExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req toolExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, sandboxlimits.ToolExecRequestBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid tool exec request")
		return
	}
	resp := s.executeTool(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}

func (s Server) executeTool(ctx context.Context, req toolExecRequest) toolExecResponse {
	switch req.Operation {
	case toolOpExec:
		return s.executeCommand(ctx, req)
	case toolOpReadFile:
		return s.executeReadFile(req, false)
	case toolOpReadFileBuffer:
		return s.executeReadFile(req, true)
	case toolOpWriteFile:
		return s.executeWriteFile(req)
	case toolOpStat:
		return s.executeStat(req)
	case toolOpReaddir:
		return s.executeReaddir(req)
	case toolOpExists:
		return s.executeExists(req)
	case toolOpMkdir:
		return s.executeMkdir(req)
	case toolOpRm:
		return s.executeRm(req)
	default:
		return toolExecResponse{Error: "unsupported tool operation"}
	}
}

func (s Server) executeCommand(ctx context.Context, req toolExecRequest) toolExecResponse {
	command := strings.TrimSpace(req.Command)
	if command == "" {
		return toolExecResponse{Error: "command is required"}
	}
	_, cwd, err := s.toolWorkspacePath(req.Cwd)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if req.TimeoutMillis > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMillis)*time.Millisecond)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, command, req.Args...)
	cmd.Dir = cwd
	env, err := s.toolCommandEnv(req.Env)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	stdout := newCommandOutputBuffer(maxCommandOutputStreamBytes)
	stderr := newCommandOutputBuffer(maxCommandOutputStreamBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	resp := toolExecResponse{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		resp.TimedOut = true
		resp.ExitCode = -1
		resp.Error = "command timed out"
		return resp
	}
	if err == nil {
		return resp
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		resp.ExitCode = exitErr.ExitCode()
		return resp
	}
	resp.ExitCode = -1
	resp.Error = err.Error()
	return resp
}

func (s Server) toolCommandEnv(requestEnv map[string]string) ([]string, error) {
	envMap := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			envMap[key] = value
		}
	}
	runtimeEnv, err := s.readToolRuntimeEnv()
	if err != nil {
		return nil, err
	}
	for key, value := range runtimeEnv {
		if toolCommandRuntimeEnvAllowlist[key] {
			envMap[key] = value
		}
	}
	for key, value := range requestEnv {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, "=") {
			return nil, fmt.Errorf("invalid environment variable name")
		}
		envMap[key] = value
	}
	env := make([]string, 0, len(envMap))
	for key, value := range envMap {
		env = append(env, key+"="+value)
	}
	return env, nil
}

func (s Server) readToolRuntimeEnv() (map[string]string, error) {
	path := filepath.Join(s.runtimePrivateRoot(), "env.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime env: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode runtime env: %w", err)
	}
	return env, nil
}

func (s Server) executeReadFile(req toolExecRequest, buffer bool) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	file, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		if !os.IsNotExist(err) {
			return toolExecResponse{Error: err.Error()}
		}
		return toolExecResponse{Error: "workspace file not found"}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return toolExecResponse{Error: "workspace path is not a regular file"}
	}
	if info.Size() > sandboxlimits.ToolReadRawBytes {
		return toolExecResponse{Error: fmt.Sprintf("workspace file exceeds read limit of %d bytes", sandboxlimits.ToolReadRawBytes)}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if buffer {
		resp := toolExecResponse{ContentBase64: base64.StdEncoding.EncodeToString(data)}
		if marshaledToolResponseExceedsLimit(resp) {
			return toolExecResponse{Error: fmt.Sprintf("readFileBuffer response exceeds control response limit of %d bytes", sandboxlimits.ControlClientResponseBytes)}
		}
		return resp
	}
	resp := toolExecResponse{Content: string(data)}
	if marshaledToolResponseExceedsLimit(resp) {
		return toolExecResponse{Error: fmt.Sprintf("readFile response exceeds control response limit of %d bytes", sandboxlimits.ControlClientResponseBytes)}
	}
	return resp
}

func marshaledToolResponseExceedsLimit(resp toolExecResponse) bool {
	data, err := json.Marshal(resp)
	if err != nil {
		return true
	}
	return int64(len(data)) > sandboxlimits.ControlClientResponseBytes
}

func (s Server) executeWriteFile(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	var data []byte
	if strings.TrimSpace(req.ContentBase64) != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.ContentBase64))
		if err != nil {
			return toolExecResponse{Error: "contentBase64 must be valid base64"}
		}
		data = decoded
	} else {
		data = []byte(req.Content)
	}
	parent, err := openWorkspaceParent(root, target, true)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	defer parent.close()
	tmp, tmpName, err := createWorkspaceTemp(parent)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			unlinkWorkspaceTemp(parent, tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if _, err := tmp.Write(data); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if err := commitWorkspaceTemp(parent, tmpName); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	cleanup = false
	return toolExecResponse{}
}

func (s Server) executeStat(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	file, err := openWorkspaceTarget(root, target, unix.O_PATH)
	if err != nil {
		if !os.IsNotExist(err) {
			return toolExecResponse{Error: err.Error()}
		}
		return toolExecResponse{Error: "workspace path not found"}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	return toolExecResponse{Stat: fileStatMap(root, target, info)}
}

func (s Server) executeReaddir(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	directory, err := openWorkspaceTarget(root, target, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		if !os.IsNotExist(err) {
			return toolExecResponse{Error: err.Error()}
		}
		return toolExecResponse{Error: "workspace path not found"}
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	resp := toolExecResponse{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		kind := "file"
		if info.IsDir() {
			kind = "dir"
		}
		resp.Entries = append(resp.Entries, toolExecutorDirEntry{
			Name:  entry.Name(),
			Type:  kind,
			Size:  info.Size(),
			Mtime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return resp
}

func (s Server) executeExists(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	file, err := openWorkspaceTarget(root, target, unix.O_PATH)
	if os.IsNotExist(err) {
		exists := false
		return toolExecResponse{Exists: &exists}
	}
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	_ = file.Close()
	exists := true
	return toolExecResponse{Exists: &exists}
}

func (s Server) executeMkdir(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if req.Recursive {
		directory, openErr := openOrCreateWorkspaceDirectory(root, target)
		if openErr != nil {
			return toolExecResponse{Error: openErr.Error()}
		}
		if closeErr := directory.Close(); closeErr != nil {
			return toolExecResponse{Error: closeErr.Error()}
		}
		return toolExecResponse{}
	}
	parent, err := openWorkspaceParent(root, target, false)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	defer parent.close()
	if err := rejectWorkspaceSymlinkAt(parent.parentFD, parent.base); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if err := unix.Mkdirat(parent.parentFD, parent.base, 0o755); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if err := unix.Fsync(parent.parentFD); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	return toolExecResponse{}
}

func (s Server) executeRm(req toolExecRequest) toolExecResponse {
	root, target, err := s.toolWorkspacePath(req.Path)
	if err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	if target == root {
		return toolExecResponse{Error: "refusing to remove workspace root"}
	}
	parent, err := openWorkspaceParent(root, target, false)
	if err != nil {
		if req.Force && os.IsNotExist(err) {
			return toolExecResponse{}
		}
		return toolExecResponse{Error: err.Error()}
	}
	defer parent.close()
	if err := removeWorkspaceEntryAt(parent.parentFD, parent.base, req.Recursive); err != nil {
		if req.Force && os.IsNotExist(err) {
			return toolExecResponse{}
		}
		return toolExecResponse{Error: err.Error()}
	}
	if err := unix.Fsync(parent.parentFD); err != nil {
		return toolExecResponse{Error: err.Error()}
	}
	return toolExecResponse{}
}

type RestoreHardenInput struct {
	HostTime time.Time
	Entropy  []byte
}

type LinuxRestoreHardener struct{}

func (LinuxRestoreHardener) Harden(_ context.Context, input RestoreHardenInput) error {
	if len(input.Entropy) == 0 {
		return fmt.Errorf("fresh entropy is required")
	}
	f, err := os.OpenFile("/dev/urandom", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/urandom: %w", err)
	}
	if _, err := f.Write(input.Entropy); err != nil {
		_ = f.Close()
		return fmt.Errorf("mix entropy: %w", err)
	}
	// Force an immediate CSPRNG reseed so cloned restores get divergent entropy.
	// Firecracker's minimal kernel runs without ACPI, so CONFIG_VMGENID (an ACPI
	// driver) is unavailable; this explicit reseed of host-supplied entropy is the
	// re-individuation mechanism rather than relying on an automatic kernel reseed.
	const rndReseedCRNG = 0x5207 // RNDRESEEDCRNG
	_ = unix.IoctlSetInt(int(f.Fd()), rndReseedCRNG, 0)
	if err := f.Close(); err != nil {
		return fmt.Errorf("close /dev/urandom: %w", err)
	}
	tv := unix.NsecToTimeval(input.HostTime.UnixNano())
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("set guest clock: %w", err)
	}
	return nil
}

func (s Server) applySecrets(bundle SecretBundle) error {
	root := s.runtimePrivateRoot()
	if err := os.MkdirAll(root, 0o711); err != nil {
		return fmt.Errorf("create runtime private dir: %w", err)
	}
	if err := os.Chmod(root, 0o711); err != nil {
		return fmt.Errorf("chmod runtime private dir: %w", err)
	}
	if len(bundle.Env) > 0 {
		data, err := json.MarshalIndent(bundle.Env, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal secret env: %w", err)
		}
		if err := atomicWritePrivate(filepath.Join(root, "env.json"), data); err != nil {
			return fmt.Errorf("write secret env: %w", err)
		}
	}
	for name, content := range bundle.Files {
		if hasParentSegment(name) {
			return fmt.Errorf("invalid secret file path")
		}
		cleaned := filepath.Clean("/" + strings.TrimPrefix(name, "/"))
		if cleaned == "/" {
			return fmt.Errorf("invalid secret file path")
		}
		target := filepath.Join(root, strings.TrimPrefix(cleaned, string(filepath.Separator)))
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("secret file path escapes private dir")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create secret file dir: %w", err)
		}
		perm := os.FileMode(0o600)
		switch filepath.Base(target) {
		case "proxy-ca.crt":
			perm = 0o644
		case "github-askpass":
			perm = 0o700
		}
		if err := atomicWriteFile(target, []byte(content), perm); err != nil {
			return fmt.Errorf("write secret file: %w", err)
		}
	}
	if !bundle.RuntimeContextProjection.IsZero() {
		if err := s.applyRuntimeContextProjection(bundle.RuntimeContextProjection); err != nil {
			return err
		}
	}
	return nil
}

func (s Server) applyRuntimeContextProjection(projection runtimecontext.Projection) error {
	projection, err := runtimecontext.Canonicalize(projection)
	if err != nil {
		return fmt.Errorf("validate runtime context projection: %w", err)
	}
	workspaceRoot, oauthDir, err := s.ensureRuntimeContextDirs()
	if err != nil {
		return err
	}
	configRoot := filepath.Join(workspaceRoot, "config")
	if err := scrubLegacyOAuthFiles(oauthDir); err != nil {
		return err
	}
	currentFiles := map[string]struct{}{}
	for _, file := range projection.Files {
		currentFiles[file.Path] = struct{}{}
	}
	deletePaths := map[string]struct{}{}
	if strings.TrimSpace(projection.EffectivePolicy) == registry.CredentialPolicyNone {
		for _, path := range runtimecontext.AllProjectionPaths() {
			deletePaths[path] = struct{}{}
		}
	}
	for _, path := range projection.OmittedPaths {
		deletePaths[path] = struct{}{}
	}
	for _, path := range projection.MigratedServicePaths {
		if _, ok := currentFiles[path]; !ok {
			deletePaths[path] = struct{}{}
		}
	}
	for path := range deletePaths {
		if err := runtimecontext.ValidatePath(path); err != nil {
			return err
		}
		if err := removeContextPath(configRoot, path); err != nil {
			return err
		}
	}
	for _, file := range projection.Files {
		target, err := runtimeContextTarget(configRoot, file.Path)
		if err != nil {
			return err
		}
		if err := atomicWriteFile(target, []byte(file.Content), 0o600); err != nil {
			return fmt.Errorf("write runtime context file %s: %w", file.Path, err)
		}
	}
	return nil
}

func (s Server) ensureRuntimeContextDirs() (string, string, error) {
	workspaceRoot := s.workspaceRoot()
	if err := ensureRootDir(workspaceRoot, 0o755); err != nil {
		return "", "", fmt.Errorf("validate workspace root: %w", err)
	}
	configDir, err := ensureRealChildDir(workspaceRoot, "config", 0o755)
	if err != nil {
		return "", "", fmt.Errorf("prepare workspace config dir: %w", err)
	}
	oauthDir, err := ensureRealChildDir(configDir, "oauth", 0o700)
	if err != nil {
		return "", "", fmt.Errorf("prepare workspace oauth dir: %w", err)
	}
	return workspaceRoot, oauthDir, nil
}

func ensureRootDir(path string, perm os.FileMode) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, perm); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return os.Chmod(path, perm)
}

func ensureRealChildDir(parent, name string, perm os.FileMode) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid child dir %q", name)
	}
	if info, err := os.Lstat(parent); err != nil {
		return "", err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s is not a real directory", parent)
	}
	path := filepath.Join(parent, name)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, perm); err != nil {
			return "", err
		}
		return path, os.Chmod(path, perm)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return "", err
		}
		if err := os.Mkdir(path, perm); err != nil {
			return "", err
		}
		return path, os.Chmod(path, perm)
	}
	return path, os.Chmod(path, perm)
}

func scrubLegacyOAuthFiles(oauthDir string) error {
	rawNames := []string{
		"github.json",
		"jira.json",
		"notion.json",
		"tempo.json",
		"gitlab.json",
		"google-workspace.json",
		"google-workspace.credentials.json",
	}
	for _, name := range rawNames {
		if err := removeFileIfExists(filepath.Join(oauthDir, name)); err != nil {
			return fmt.Errorf("remove legacy OAuth file %s: %w", name, err)
		}
	}
	entries, err := os.ReadDir(oauthDir)
	if err != nil {
		return fmt.Errorf("read OAuth config dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		for _, raw := range rawNames {
			matched, err := filepath.Match("."+raw+".*.tmp", name)
			if err != nil {
				return err
			}
			if matched {
				if err := removeFileIfExists(filepath.Join(oauthDir, name)); err != nil {
					return fmt.Errorf("remove legacy OAuth temp file %s: %w", name, err)
				}
				break
			}
		}
	}
	return nil
}

func removeContextPath(configRoot, path string) error {
	target, err := runtimeContextTarget(configRoot, path)
	if err != nil {
		return err
	}
	if err := removeFileIfExists(target); err != nil {
		return fmt.Errorf("remove runtime context file %s: %w", path, err)
	}
	return nil
}

func runtimeContextTarget(configRoot, path string) (string, error) {
	if err := runtimecontext.ValidatePath(path); err != nil {
		return "", err
	}
	target := filepath.Join(configRoot, filepath.FromSlash(path))
	rel, err := filepath.Rel(configRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("runtime context path escapes config root")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("create runtime context dir: %w", err)
	}
	return target, nil
}

func removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s Server) workspaceRoot() string {
	root := strings.TrimSpace(s.WorkspaceDir)
	if root == "" {
		root = "/workspace"
	}
	return filepath.Clean(root)
}

func (s Server) runtimePrivateRoot() string {
	root := strings.TrimSpace(s.RuntimePrivateDir)
	if root == "" {
		root = "/runtime-private"
	}
	return filepath.Clean(root)
}

func atomicWritePrivate(path string, data []byte) error {
	return atomicWriteFile(path, data, 0o600)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func fileTransferLimitFromQuery(r *http.Request, defaultLimit int64) (int64, error) {
	maxBytes := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("maxBytes")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("maxBytes must be a positive integer")
		}
		if parsed < maxBytes {
			maxBytes = parsed
		}
	}
	return maxBytes, nil
}

var errTransferTooLarge = errors.New("workspace transfer too large")
var errUnsafeWorkspacePath = errors.New("workspace path contains a symlink or escapes root")

type countingLimitReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (r *countingLimitReader) Read(p []byte) (int, error) {
	if r.n > r.max {
		return 0, errTransferTooLarge
	}
	remaining := r.max - r.n
	if remaining < int64(len(p)) {
		p = p[:remaining+1]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if r.n > r.max {
		return n, errTransferTooLarge
	}
	return n, err
}

func (s Server) workspacePath(raw string) (string, string, error) {
	root := s.workspaceRoot()
	if hasParentSegment(raw) {
		return "", "", fmt.Errorf("workspace path escapes root")
	}
	cleaned := filepath.Clean("/" + strings.TrimPrefix(raw, "/"))
	target := filepath.Join(root, strings.TrimPrefix(cleaned, string(filepath.Separator)))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", fmt.Errorf("invalid workspace path")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("workspace path escapes root")
	}
	return root, target, nil
}

type workspaceParent struct {
	rootFD   int
	parentFD int
	base     string
}

func (p *workspaceParent) close() {
	if p == nil {
		return
	}
	if p.parentFD >= 0 && p.parentFD != p.rootFD {
		_ = unix.Close(p.parentFD)
	}
	if p.rootFD >= 0 {
		_ = unix.Close(p.rootFD)
	}
}

func openWorkspaceRoot(root string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, normalizeWorkspaceOpenError(err)
	}
	return fd, nil
}

func workspaceRelativePath(root, target string) (string, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errUnsafeWorkspacePath
	}
	return rel, nil
}

func workspacePathParts(rel string) ([]string, error) {
	if rel == "." {
		return nil, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errUnsafeWorkspacePath
		}
	}
	return parts, nil
}

func normalizeWorkspaceOpenError(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return errUnsafeWorkspacePath
	}
	return err
}

func openWorkspaceTarget(root, target string, flags int) (*os.File, error) {
	rel, err := workspaceRelativePath(root, target)
	if err != nil {
		return nil, err
	}
	parts, err := workspacePathParts(rel)
	if err != nil {
		return nil, err
	}
	rootFD, err := openWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return os.NewFile(uintptr(rootFD), root), nil
	}
	currentFD := rootFD
	for index, part := range parts {
		openFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index == len(parts)-1 {
			openFlags = flags | unix.O_CLOEXEC | unix.O_NOFOLLOW
		}
		fd, openErr := unix.Openat(currentFD, part, openFlags, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return nil, normalizeWorkspaceOpenError(openErr)
		}
		currentFD = fd
	}
	_ = unix.Close(rootFD)
	file := os.NewFile(uintptr(currentFD), target)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, errUnsafeWorkspacePath
	}
	return file, nil
}

func openWorkspaceParent(root, target string, createParents bool) (*workspaceParent, error) {
	rel, err := workspaceRelativePath(root, target)
	if err != nil {
		return nil, err
	}
	parts, err := workspacePathParts(rel)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errors.New("workspace root has no parent entry")
	}
	rootFD, err := openWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	currentFD := rootFD
	for _, part := range parts[:len(parts)-1] {
		fd, openErr := unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && createParents {
			if mkdirErr := unix.Mkdirat(currentFD, part, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				if currentFD != rootFD {
					_ = unix.Close(currentFD)
				}
				_ = unix.Close(rootFD)
				return nil, mkdirErr
			}
			if syncErr := unix.Fsync(currentFD); syncErr != nil {
				if currentFD != rootFD {
					_ = unix.Close(currentFD)
				}
				_ = unix.Close(rootFD)
				return nil, syncErr
			}
			fd, openErr = unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			if currentFD != rootFD {
				_ = unix.Close(currentFD)
			}
			_ = unix.Close(rootFD)
			return nil, normalizeWorkspaceOpenError(openErr)
		}
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		currentFD = fd
	}
	return &workspaceParent{rootFD: rootFD, parentFD: currentFD, base: parts[len(parts)-1]}, nil
}

func rejectWorkspaceSymlinkAt(parentFD int, name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errUnsafeWorkspacePath
	}
	return nil
}

func createWorkspaceTemp(parent *workspaceParent) (*os.File, string, error) {
	if err := rejectWorkspaceSymlinkAt(parent.parentFD, parent.base); err != nil {
		return nil, "", err
	}
	for attempt := 0; attempt < 32; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", err
		}
		name := fmt.Sprintf(".%s.%x.tmp", parent.base, suffix)
		fd, err := unix.Openat(parent.parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", normalizeWorkspaceOpenError(err)
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", errors.New("create workspace temporary file: exhausted unique names")
}

func commitWorkspaceTemp(parent *workspaceParent, temporaryName string) error {
	if err := rejectWorkspaceSymlinkAt(parent.parentFD, parent.base); err != nil {
		return err
	}
	if err := unix.Renameat(parent.parentFD, temporaryName, parent.parentFD, parent.base); err != nil {
		return err
	}
	return unix.Fsync(parent.parentFD)
}

func unlinkWorkspaceTemp(parent *workspaceParent, temporaryName string) {
	if parent != nil && temporaryName != "" {
		_ = unix.Unlinkat(parent.parentFD, temporaryName, 0)
	}
}

func openOrCreateWorkspaceDirectory(root, target string) (*os.File, error) {
	rel, err := workspaceRelativePath(root, target)
	if err != nil {
		return nil, err
	}
	parts, err := workspacePathParts(rel)
	if err != nil {
		return nil, err
	}
	rootFD, err := openWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return os.NewFile(uintptr(rootFD), root), nil
	}
	currentFD := rootFD
	for _, part := range parts {
		fd, openErr := unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(currentFD, part, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				if currentFD != rootFD {
					_ = unix.Close(currentFD)
				}
				_ = unix.Close(rootFD)
				return nil, mkdirErr
			}
			if syncErr := unix.Fsync(currentFD); syncErr != nil {
				if currentFD != rootFD {
					_ = unix.Close(currentFD)
				}
				_ = unix.Close(rootFD)
				return nil, syncErr
			}
			fd, openErr = unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			if currentFD != rootFD {
				_ = unix.Close(currentFD)
			}
			_ = unix.Close(rootFD)
			return nil, normalizeWorkspaceOpenError(openErr)
		}
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		currentFD = fd
	}
	_ = unix.Close(rootFD)
	return os.NewFile(uintptr(currentFD), target), nil
}

func removeWorkspaceEntryAt(parentFD int, name string, recursive bool) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errUnsafeWorkspacePath
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parentFD, name, 0)
	}
	if !recursive {
		return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
	}
	childFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return normalizeWorkspaceOpenError(err)
	}
	child := os.NewFile(uintptr(childFD), name)
	entries, err := child.Readdirnames(-1)
	if err != nil {
		_ = child.Close()
		return err
	}
	for _, entry := range entries {
		if err := removeWorkspaceEntryAt(childFD, entry, true); err != nil {
			_ = child.Close()
			return err
		}
	}
	if err := child.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func (s Server) toolWorkspacePath(raw string) (string, string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", "", fmt.Errorf("invalid workspace path")
	}
	if strings.HasPrefix(raw, string(filepath.Separator)) && filepath.Clean(raw) != string(filepath.Separator) {
		return "", "", fmt.Errorf("workspace path escapes root")
	}
	depth := 0
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch part {
		case "", ".":
			continue
		case "..":
			depth--
		default:
			depth++
		}
		if depth < 0 {
			return "", "", fmt.Errorf("workspace path escapes root")
		}
	}
	root := s.workspaceRoot()
	cleaned := filepath.Clean("/" + strings.TrimPrefix(raw, "/"))
	target := filepath.Join(root, strings.TrimPrefix(cleaned, string(filepath.Separator)))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", fmt.Errorf("invalid workspace path")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("workspace path escapes root")
	}
	return root, target, nil
}

func fileStatMap(root, target string, info os.FileInfo) map[string]any {
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		rel = filepath.Base(target)
	}
	return map[string]any{
		"path":  filepath.ToSlash(rel),
		"type":  kind,
		"size":  info.Size(),
		"mtime": info.ModTime().UTC().Format(time.RFC3339),
		"mode":  info.Mode().Perm().String(),
	}
}

func hasParentSegment(raw string) bool {
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
