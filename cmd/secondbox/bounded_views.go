package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/SecondStack-AI/SecondBox/internal/cliui"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

var boundedOperations = map[string]bool{
	"listProfiles": true, "getProfile": true,
	"listRunnerPools": true, "getRunnerPool": true,
	"listRunners": true, "getRunner": true,
	"listSandboxes": true, "getSandbox": true,
	"listSandboxSnapshots": true, "getSnapshot": true,
	"listSandboxArtifacts": true, "getArtifact": true,
	"getSandboxLease": true, "getSandboxPortSession": true,
	"inspectSandbox": true, "getOperation": true,
}

func renderBoundedOperation(operationID string, content []byte, renderer cliui.Renderer) error {
	switch operationID {
	case "listProfiles":
		var page secondboxclient.ProfilePage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"name": item.Name, "state": item.State, "revision": strconv.FormatInt(item.Revision, 10), "current": strconv.FormatInt(item.CurrentRevision.Number, 10)})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "name", Title: "NAME", Priority: 0, MinWidth: 12}, {Key: "state", Title: "STATE", Priority: 1, MinWidth: 8}, {Key: "current", Title: "PROFILE REV", Priority: 2, MinWidth: 8}, {Key: "revision", Title: "RESOURCE REV", Priority: 3, MinWidth: 8}}, Rows: rows, Empty: "No Profiles found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "listRunnerPools":
		var page secondboxclient.RunnerPoolPage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"name": item.Name, "state": item.State, "ready": strconv.FormatInt(item.ReadyRunnerCount, 10), "architectures": strings.Join(item.Architectures, ",")})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "name", Title: "NAME", Priority: 0, MinWidth: 12}, {Key: "state", Title: "STATE", Priority: 1, MinWidth: 8}, {Key: "ready", Title: "READY", Priority: 2, MinWidth: 5}, {Key: "architectures", Title: "ARCHITECTURES", Priority: 3, MinWidth: 12}}, Rows: rows, Empty: "No RunnerPools found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "listRunners":
		var page secondboxclient.RunnerPage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"id": item.ID, "name": item.Name, "pool": item.PoolName, "state": item.State})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 16}, {Key: "name", Title: "NAME", Priority: 1, MinWidth: 10}, {Key: "pool", Title: "POOL", Priority: 2, MinWidth: 10}, {Key: "state", Title: "STATE", Priority: 3, MinWidth: 8}}, Rows: rows, Empty: "No Runners found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "listSandboxes":
		var page secondboxclient.SandboxPage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"id": item.ID, "profile": item.Profile, "state": item.State, "generation": strconv.FormatInt(item.Generation, 10), "revision": strconv.FormatInt(item.Revision, 10)})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 16}, {Key: "profile", Title: "PROFILE", Priority: 1, MinWidth: 12}, {Key: "state", Title: "STATE", Priority: 2, MinWidth: 8}, {Key: "generation", Title: "GEN", Priority: 3, MinWidth: 4}, {Key: "revision", Title: "REV", Priority: 4, MinWidth: 4}}, Rows: rows, Empty: "No Sandboxes found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "listSandboxSnapshots":
		var page secondboxclient.SnapshotPage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"id": item.ID, "name": item.Name, "state": item.State, "sandbox": item.SandboxID, "size": strconv.FormatInt(item.SizeBytes, 10)})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 16}, {Key: "name", Title: "NAME", Priority: 1, MinWidth: 10}, {Key: "state", Title: "STATE", Priority: 2, MinWidth: 8}, {Key: "sandbox", Title: "SANDBOX", Priority: 3, MinWidth: 16}, {Key: "size", Title: "BYTES", Priority: 4, MinWidth: 8}}, Rows: rows, Empty: "No Snapshots found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "listSandboxArtifacts":
		var page secondboxclient.ArtifactPage
		if err := decodeView(content, &page); err != nil {
			return err
		}
		rows := make([]cliui.Row, 0, len(page.Items))
		for _, item := range page.Items {
			rows = append(rows, cliui.Row{"id": string(item.ID), "name": item.Name, "sandbox": string(item.SandboxID), "type": item.MediaType, "size": strconv.FormatInt(item.SizeBytes, 10)})
		}
		return renderer.WriteTable(cliui.Table{Columns: []cliui.Column{{Key: "id", Title: "ID", Priority: 0, MinWidth: 16}, {Key: "name", Title: "NAME", Priority: 1, MinWidth: 10}, {Key: "sandbox", Title: "SANDBOX", Priority: 2, MinWidth: 16}, {Key: "type", Title: "MEDIA TYPE", Priority: 3, MinWidth: 12}, {Key: "size", Title: "BYTES", Priority: 4, MinWidth: 8}}, Rows: rows, Empty: "No Artifacts found.", ContinuationCursor: pageCursor(page.NextCursor)})
	case "getProfile":
		var item secondboxclient.Profile
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Profile", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "Name", Value: item.Name}, {Key: "State", Value: item.State}, {Key: "Current revision", Value: strconv.FormatInt(item.CurrentRevision.Number, 10)}, {Key: "Resource revision", Value: strconv.FormatInt(item.Revision, 10)}}})
	case "getRunnerPool":
		var item secondboxclient.RunnerPool
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "RunnerPool", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "Name", Value: item.Name}, {Key: "State", Value: item.State}, {Key: "Ready Runners", Value: strconv.FormatInt(item.ReadyRunnerCount, 10)}, {Key: "Architectures", Value: strings.Join(item.Architectures, ",")}, {Key: "Revision", Value: strconv.FormatInt(item.Revision, 10)}}})
	case "getRunner":
		var item secondboxclient.Runner
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Runner", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: item.ID}, {Key: "Name", Value: item.Name}, {Key: "Pool", Value: item.PoolName}, {Key: "State", Value: item.State}, {Key: "Credential", Value: item.CredentialState}, {Key: "Revision", Value: strconv.FormatInt(item.Revision, 10)}}})
	case "getSandbox":
		var item secondboxclient.Sandbox
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Sandbox", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: item.ID}, {Key: "Profile", Value: item.Profile}, {Key: "State", Value: item.State}, {Key: "Desired state", Value: item.DesiredState}, {Key: "Generation", Value: strconv.FormatInt(item.Generation, 10)}, {Key: "Revision", Value: strconv.FormatInt(item.Revision, 10)}}})
	case "getSnapshot":
		var item secondboxclient.Snapshot
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Snapshot", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: item.ID}, {Key: "Name", Value: item.Name}, {Key: "Sandbox", Value: item.SandboxID}, {Key: "State", Value: item.State}, {Key: "Generation", Value: strconv.FormatInt(item.SourceGeneration, 10)}, {Key: "Size bytes", Value: strconv.FormatInt(item.SizeBytes, 10)}}})
	case "getArtifact":
		var item secondboxclient.Artifact
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Artifact", Status: cliui.StatusComplete, Pairs: []cliui.Pair{{Key: "ID", Value: string(item.ID)}, {Key: "Name", Value: item.Name}, {Key: "Sandbox", Value: string(item.SandboxID)}, {Key: "Media type", Value: item.MediaType}, {Key: "Size bytes", Value: strconv.FormatInt(item.SizeBytes, 10)}, {Key: "SHA-256", Value: item.SHA256}}})
	case "getSandboxLease":
		var item secondboxclient.Lease
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Lease", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: item.ID}, {Key: "Sandbox", Value: item.SandboxID}, {Key: "State", Value: item.State}, {Key: "Generation", Value: strconv.FormatInt(item.Generation, 10)}, {Key: "Expires", Value: item.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")}}})
	case "getSandboxPortSession":
		var item secondboxclient.PortSession
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Port", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: string(item.ID)}, {Key: "Name", Value: item.Name}, {Key: "Sandbox", Value: string(item.SandboxID)}, {Key: "State", Value: item.State}, {Key: "Protocol", Value: item.Protocol}, {Key: "Endpoint", Value: item.Endpoint}}})
	case "inspectSandbox":
		var item secondboxclient.SandboxInspection
		if err := decodeView(content, &item); err != nil {
			return err
		}
		status := cliui.StatusWarning
		if item.GuestHealthy {
			status = cliui.StatusComplete
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Sandbox inspection", Status: status, Pairs: []cliui.Pair{{Key: "Sandbox", Value: string(item.SandboxID)}, {Key: "Generation", Value: strconv.FormatInt(item.Generation, 10)}, {Key: "Guest healthy", Value: strconv.FormatBool(item.GuestHealthy)}, {Key: "Active sessions", Value: strconv.FormatInt(item.ActiveSessions, 10)}, {Key: "Observed", Value: item.ObservedAt.Format("2006-01-02T15:04:05Z07:00")}}})
	case "getOperation":
		var item secondboxclient.Operation
		if err := decodeView(content, &item); err != nil {
			return err
		}
		return renderer.WriteSummary(cliui.Summary{Title: "Operation", Status: viewStatus(item.State), Pairs: []cliui.Pair{{Key: "ID", Value: item.ID}, {Key: "Kind", Value: item.Kind}, {Key: "State", Value: item.State}, {Key: "Sandbox", Value: item.SandboxID}, {Key: "Request", Value: item.RequestID}}})
	default:
		return fmt.Errorf("SecondBox CLI has no bounded view for %s", operationID)
	}
}

func decodeView(content []byte, target any) error {
	if err := json.Unmarshal(content, target); err != nil {
		return fmt.Errorf("SecondBox CLI decode human view: %w", err)
	}
	return nil
}

func pageCursor(cursor *string) string {
	if cursor == nil {
		return ""
	}
	return *cursor
}

func viewStatus(value string) cliui.Status {
	switch value {
	case "ready", "enabled", "open", "succeeded", "active":
		return cliui.StatusComplete
	case "failed", "offline", "cancelled", "deleted":
		return cliui.StatusFailed
	case "draining", "stopping", "disabled":
		return cliui.StatusWarning
	case "creating", "starting", "running", "pending":
		return cliui.StatusActive
	default:
		return cliui.StatusPending
	}
}
