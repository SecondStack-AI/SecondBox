package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func runResourcesCommand(ctx context.Context, session cliSession, action string, args []string, output io.Writer, httpClient *http.Client) error {
	flags := flag.NewFlagSet("resources "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "desired resource JSON document")
	bundle := flags.String("standard-bundle", "", "release-owned standard bundle")
	manifestPath := flags.String("artifact-manifest", "", "verified release artifact manifest")
	pool := flags.String("pool", "", "deployment RunnerPool name")
	architectures := flags.String("architectures", "amd64", "comma-separated architecture inventory")
	capabilities := flags.String("capabilities", "exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy", "comma-separated RunnerPool capabilities")
	state := flags.String("state", secondboxclient.RunnerPoolStateReady, "desired RunnerPool state")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("SecondBox resources %s options: %w", action, err)
	}
	if flags.NArg() != 0 || ((*file == "") == (*bundle == "")) {
		return fmt.Errorf("SecondBox resources %s requires exactly one of --file or --standard-bundle", action)
	}
	if session.url == "" || session.token == "" {
		return errors.New("SecondBox resources requires --url and --token" + sessionSourceHint)
	}
	document, err := loadResourceDocument(*file, *bundle, *manifestPath, *pool, *architectures, *capabilities, *state)
	if err != nil {
		return err
	}
	client, err := secondboxclient.NewSecondBoxClient(session.url, session.token, httpClient)
	if err != nil {
		return err
	}
	var report resourceapply.Report
	if action == "check" {
		report, err = resourceapply.Check(ctx, client, document)
	} else {
		report, err = resourceapply.Apply(ctx, client, document)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func loadResourceDocument(file, bundle, manifestPath, pool, architectureCSV, capabilityCSV, state string) (resourceapply.Document, error) {
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return resourceapply.Document{}, fmt.Errorf("SecondBox resource document read failed: %w", err)
		}
		return resourceapply.Decode(data)
	}
	if manifestPath == "" || pool == "" {
		return resourceapply.Document{}, errors.New("SecondBox standard bundle requires --artifact-manifest and --pool")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return resourceapply.Document{}, fmt.Errorf("SecondBox artifact manifest read failed: %w", err)
	}
	manifest, err := releasecontract.DecodeArtifactManifest(data)
	if err != nil {
		return resourceapply.Document{}, err
	}
	binding := standardresources.PoolBinding{Name: pool, Architectures: splitCSV(architectureCSV), Capabilities: splitCSV(capabilityCSV), State: state}
	return standardresources.Build(manifest, standardresources.Selection{Bundles: []string{bundle}, Pools: map[string]standardresources.PoolBinding{bundle: binding}})
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
