//go:build scenario_live

package scenario_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const scenarioFileTransferMaxBytes = 1 << 20

func TestScenarioTerminalFilesystemAndArtifacts(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	profile := createScenarioProfile(
		t,
		fixture,
		"scenario-data-paths",
		scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning),
	)
	handle, _ := createScenarioSandbox(t, fixture, profile, "data-paths")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateReady)

	t.Run("terminal dimensions detach reconnect and cancellation", func(t *testing.T) {
		lease := scenarioJSON[contracts.Lease](
			t,
			ctx,
			fixture.subject,
			"acquireSandboxLease",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				Headers:        scenarioDataPlaneHeaders(handle, uniqueScenarioKey(t, "terminal-lease")),
				Body: scenarioBody(t, contracts.AcquireLeaseRequest{
					DurationSeconds: 60,
				}),
			},
		)
		session, err := handle.CreateTerminal(
			ctx,
			secondboxclient.CreateTerminalRequest{
				Command: secondboxclient.Command{
					ShellCommand: &secondboxclient.ShellCommand{
						Mode: "shell",
						Command: "IFS= read -r first; printf 'input=%s dims=' \"$first\"; stty size; " +
							"IFS= read -r second; printf 'input=%s resized=' \"$second\"; stty size; " +
							"IFS= read -r third; printf 'resumed=%s\\n' \"$third\"; sleep 60",
					},
				},
				Environment:          secondboxclient.StringMap{},
				Rows:                 24,
				Columns:              80,
				DeadlineMilliseconds: 60000,
				Detachable:           true,
			},
			uniqueScenarioKey(t, "terminal-create"),
			lease.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := handle.ConnectTerminal(ctx, session, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := terminal.GrantOutput(65536); err != nil {
			t.Fatal(err)
		}
		if err := terminal.SendInput([]byte("terminal-marker\n")); err != nil {
			t.Fatal(err)
		}
		requireScenarioTerminalText(t, terminal, "input=terminal-marker dims=24 80")
		if err := terminal.Resize(31, 97); err != nil {
			t.Fatal(err)
		}
		if err := terminal.SendInput([]byte("resize-marker\n")); err != nil {
			t.Fatal(err)
		}
		requireScenarioTerminalText(t, terminal, "input=resize-marker resized=31 97")
		if err := terminal.Close(); err != nil {
			t.Fatal(err)
		}

		reconnect := waitForScenarioTerminalState(
			t,
			ctx,
			handle,
			session.ID,
			secondboxclient.SessionStateDetached,
		)
		terminal, err = handle.ConnectTerminal(ctx, reconnect, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer terminal.Close()
		if err := terminal.SendInput([]byte("resumed-ok\n")); err != nil {
			t.Fatal(err)
		}
		requireScenarioTerminalText(t, terminal, "resumed=resumed-ok")
		cancelled, err := handle.CancelTerminal(
			ctx,
			session.ID,
			uniqueScenarioKey(t, "terminal-cancel"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.State != secondboxclient.SessionStateClosing &&
			cancelled.State != secondboxclient.SessionStateClosed {
			t.Fatalf("SecondBox scenario cancelled Terminal = %#v", cancelled)
		}
		outcome := requireScenarioTerminalOutcome(t, terminal)
		if outcome.ExecCancelled == nil {
			t.Fatalf("SecondBox scenario Terminal outcome = %#v", outcome)
		}
		released := scenarioJSON[contracts.Lease](
			t,
			ctx,
			fixture.subject,
			"releaseSandboxLease",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"leaseId": lease.ID},
				Headers:        scenarioHeaders(uniqueScenarioKey(t, "terminal-lease-release")),
			},
		)
		if released.State != contracts.LeaseStateReleased {
			t.Fatalf("SecondBox scenario released Terminal Lease = %#v", released)
		}
	})

	t.Run("filesystem binary and configured-boundary round trips", func(t *testing.T) {
		const directory = "scenario-data"
		scenarioVoid(
			t,
			ctx,
			fixture.subject,
			"createSandboxDirectory",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				Headers:        scenarioDataPlaneHeaders(handle, uniqueScenarioKey(t, "mkdir")),
				Body: scenarioBody(t, secondboxclient.CreateDirectoryRequest{
					Path: directory, Recursive: true,
				}),
			},
		)

		binary := []byte{0, 1, 2, 0xff, 0xfe, '\n', 'S', 'e', 'c', 'o', 'n', 'd', 'B', 'o', 'x'}
		writeScenarioFile(t, ctx, fixture.subject, handle, directory+"/binary.bin", binary)
		if got := readScenarioFile(t, ctx, fixture.subject, handle, directory+"/binary.bin"); !bytes.Equal(got, binary) {
			t.Fatalf("SecondBox scenario binary File = %v, want %v", got, binary)
		}

		boundary := make([]byte, scenarioFileTransferMaxBytes)
		for index := range boundary {
			boundary[index] = byte(index * 31)
		}
		writeScenarioFile(t, ctx, fixture.subject, handle, directory+"/boundary.bin", boundary)
		if got := readScenarioFile(t, ctx, fixture.subject, handle, directory+"/boundary.bin"); !bytes.Equal(got, boundary) {
			t.Fatalf("SecondBox scenario boundary File bytes = %d, want %d", len(got), len(boundary))
		}

		exists := scenarioJSON[secondboxclient.FileExistsResult](
			t,
			ctx,
			fixture.subject,
			"sandboxFileExists",
			secondboxclient.CallOptions{
				PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				QueryParameters: url.Values{"path": []string{directory + "/binary.bin"}},
				Headers:         handle.GenerationHeaders(""),
			},
		)
		if !exists.Exists {
			t.Fatalf("SecondBox scenario File existence = %#v", exists)
		}
		listing := scenarioJSON[secondboxclient.DirectoryListing](
			t,
			ctx,
			fixture.subject,
			"listSandboxDirectory",
			secondboxclient.CallOptions{
				PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				QueryParameters: url.Values{"path": []string{directory}},
				Headers:         handle.GenerationHeaders(""),
			},
		)
		if !scenarioListingContains(listing, directory+"/binary.bin") ||
			!scenarioListingContains(listing, directory+"/boundary.bin") {
			t.Fatalf("SecondBox scenario Directory listing = %#v", listing)
		}
		binaryHash := sha256.Sum256(binary)
		guest := executeScenarioCommand(
			t,
			ctx,
			handle,
			"test -d /workspace/scenario-data && sha256sum /workspace/scenario-data/binary.bin",
			4096,
			"filesystem-observation",
		)
		assertScenarioExited(t, guest, 0, hex.EncodeToString(binaryHash[:])+"  /workspace/scenario-data/binary.bin\n", "")

		scenarioVoid(
			t,
			ctx,
			fixture.subject,
			"removeSandboxPath",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				Headers:        scenarioDataPlaneHeaders(handle, uniqueScenarioKey(t, "remove")),
				Body: scenarioBody(t, secondboxclient.RemovePathRequest{
					Path: directory, Recursive: true, Force: false,
				}),
			},
		)
		exists = scenarioJSON[secondboxclient.FileExistsResult](
			t,
			ctx,
			fixture.subject,
			"sandboxFileExists",
			secondboxclient.CallOptions{
				PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				QueryParameters: url.Values{"path": []string{directory}},
				Headers:         handle.GenerationHeaders(""),
			},
		)
		if exists.Exists {
			t.Fatalf("SecondBox scenario removed path still exists: %#v", exists)
		}
		guest = executeScenarioCommand(
			t,
			ctx,
			handle,
			"test ! -e /workspace/scenario-data",
			1024,
			"filesystem-removal-observation",
		)
		assertScenarioExited(t, guest, 0, "", "")
	})

	t.Run("artifact object-store round trip and listing", func(t *testing.T) {
		content := []byte("SecondBox artifact\x00binary\n")
		sum := sha256.Sum256(content)
		digest := hex.EncodeToString(sum[:])
		artifact := uploadScenarioArtifact(t, ctx, fixture.subject, handle, content, digest)
		if artifact.SandboxID != string(handle.Snapshot().ID) ||
			artifact.SourceGeneration != handle.Snapshot().Generation ||
			artifact.SHA256 != digest ||
			artifact.SizeBytes != int64(len(content)) {
			t.Fatalf("SecondBox scenario uploaded Artifact = %#v", artifact)
		}
		got := scenarioJSON[contracts.Artifact](
			t,
			ctx,
			fixture.subject,
			"getArtifact",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"artifactId": artifact.ID},
			},
		)
		if got.ID != artifact.ID || got.SHA256 != digest {
			t.Fatalf("SecondBox scenario fetched Artifact = %#v", got)
		}
		downloaded, responseDigest := downloadScenarioArtifact(
			t,
			ctx,
			fixture.subject,
			artifact.ID,
		)
		if !bytes.Equal(downloaded, content) || responseDigest != scenarioHTTPDigest(sum) {
			t.Fatalf(
				"SecondBox scenario downloaded Artifact bytes=%v digest=%q",
				downloaded,
				responseDigest,
			)
		}
		page := scenarioJSON[contracts.ArtifactPage](
			t,
			ctx,
			fixture.subject,
			"listSandboxArtifacts",
			secondboxclient.CallOptions{
				PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				QueryParameters: url.Values{"limit": []string{"200"}},
			},
		)
		if !scenarioArtifactPageContains(page, artifact.ID) {
			t.Fatalf("SecondBox scenario Artifact page = %#v", page)
		}
		scenarioVoid(
			t,
			ctx,
			fixture.subject,
			"deleteArtifact",
			secondboxclient.CallOptions{
				PathParameters: map[string]string{"artifactId": artifact.ID},
				Headers:        scenarioHeaders(uniqueScenarioKey(t, "artifact-delete")),
			},
		)
		page = scenarioJSON[contracts.ArtifactPage](
			t,
			ctx,
			fixture.subject,
			"listSandboxArtifacts",
			secondboxclient.CallOptions{
				PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
				QueryParameters: url.Values{"limit": []string{"200"}},
			},
		)
		if scenarioArtifactPageContains(page, artifact.ID) {
			t.Fatalf("SecondBox scenario deleted Artifact remains listed: %#v", page)
		}
	})
}

func requireScenarioTerminalText(
	t *testing.T,
	terminal *secondboxclient.Terminal,
	want string,
) {
	t.Helper()
	var output strings.Builder
	for range 64 {
		frame, err := terminal.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if frame.StreamOutcomeFrame != nil {
			t.Fatalf(
				"SecondBox scenario Terminal ended before output %q: %s",
				want,
				describeScenarioExecOutcome(frame.StreamOutcomeFrame.Outcome),
			)
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(
			frame.TerminalOutputFrame.DataBase64,
		)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(decoded)
		if strings.Contains(strings.ReplaceAll(output.String(), "\r", ""), want) {
			return
		}
	}
	t.Fatalf("SecondBox scenario Terminal output %q lacks %q", output.String(), want)
}

func requireScenarioTerminalOutcome(
	t *testing.T,
	terminal *secondboxclient.Terminal,
) secondboxclient.ExecOutcome {
	t.Helper()
	for range 64 {
		frame, err := terminal.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if frame.StreamOutcomeFrame != nil {
			return frame.StreamOutcomeFrame.Outcome
		}
	}
	t.Fatal("SecondBox scenario Terminal did not return a terminal outcome")
	return secondboxclient.ExecOutcome{}
}

func describeScenarioExecOutcome(outcome secondboxclient.ExecOutcome) string {
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return err.Error()
	}
	return string(encoded)
}

func waitForScenarioTerminalState(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	sessionID secondboxclient.OpaqueID,
	state secondboxclient.SessionState,
) secondboxclient.TerminalSession {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		session, err := handle.GetTerminal(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if session.State == state {
			return session
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox scenario Terminal state = %s, want %s", session.State, state)
		case <-ticker.C:
		}
	}
}

func scenarioDataPlaneHeaders(
	handle *secondboxclient.SandboxHandle,
	idempotencyKey string,
) http.Header {
	headers := handle.GenerationHeaders("")
	headers.Set("Idempotency-Key", idempotencyKey)
	return headers
}

func scenarioVoid(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) {
	t.Helper()
	response, err := client.Request(ctx, operationID, options)
	if err != nil {
		t.Fatalf("SecondBox scenario %s: %v", operationID, err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("SecondBox scenario %s response close: %v", operationID, err)
	}
}

func writeScenarioFile(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	handle *secondboxclient.SandboxHandle,
	path string,
	content []byte,
) secondboxclient.FileWriteResult {
	t.Helper()
	sum := sha256.Sum256(content)
	headers := scenarioDataPlaneHeaders(handle, uniqueScenarioKey(t, "file-write"))
	headers.Set("Digest", scenarioHTTPDigest(sum))
	return scenarioJSON[secondboxclient.FileWriteResult](
		t,
		ctx,
		client,
		"writeSandboxFile",
		secondboxclient.CallOptions{
			PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
			QueryParameters: url.Values{"path": []string{path}},
			Headers:         headers,
			Body:            bytes.NewReader(content),
		},
	)
}

func readScenarioFile(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	handle *secondboxclient.SandboxHandle,
	path string,
) []byte {
	t.Helper()
	response, err := client.Request(ctx, "readSandboxFile", secondboxclient.CallOptions{
		PathParameters:  map[string]string{"sandboxId": string(handle.Snapshot().ID)},
		QueryParameters: url.Values{"path": []string{path}},
		Headers:         handle.GenerationHeaders(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("SecondBox scenario File read=%v close=%v", readErr, closeErr)
	}
	sum := sha256.Sum256(content)
	if got, want := response.Header.Get("Digest"), scenarioHTTPDigest(sum); got != want {
		t.Fatalf("SecondBox scenario File Digest = %q, want %q", got, want)
	}
	return content
}

func scenarioHTTPDigest(sum [sha256.Size]byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
}

func scenarioListingContains(listing secondboxclient.DirectoryListing, path string) bool {
	for _, entry := range listing.Entries {
		if string(entry.Path) == path {
			return true
		}
	}
	return false
}

func uploadScenarioArtifact(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	handle *secondboxclient.SandboxHandle,
	content []byte,
	digest string,
) contracts.Artifact {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writeField := func(name string, value []byte, contentType string) {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="`+name+`"`)
		if contentType != "" {
			header.Set("Content-Type", contentType)
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	writeField("name", []byte("scenario-artifact.bin"), "")
	writeField("mediaType", []byte("application/octet-stream"), "")
	writeField("sha256", []byte(digest), "")
	metadata, err := json.Marshal(map[string]string{"scenario": "data-paths"})
	if err != nil {
		t.Fatal(err)
	}
	writeField("metadata", metadata, "application/json")
	writeField("content", content, "application/octet-stream")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return scenarioJSON[contracts.Artifact](
		t,
		ctx,
		client,
		"uploadSandboxArtifact",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": string(handle.Snapshot().ID)},
			Headers:        scenarioDataPlaneHeaders(handle, uniqueScenarioKey(t, "artifact-upload")),
			Body:           &body,
			ContentType:    writer.FormDataContentType(),
		},
	)
}

func downloadScenarioArtifact(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	artifactID string,
) ([]byte, string) {
	t.Helper()
	response, err := client.Request(ctx, "downloadArtifactContent", secondboxclient.CallOptions{
		PathParameters: map[string]string{"artifactId": artifactID},
	})
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("SecondBox scenario Artifact read=%v close=%v", readErr, closeErr)
	}
	return content, response.Header.Get("Digest")
}

func scenarioArtifactPageContains(page contracts.ArtifactPage, artifactID string) bool {
	for _, artifact := range page.Items {
		if artifact.ID == artifactID {
			return true
		}
	}
	return false
}
