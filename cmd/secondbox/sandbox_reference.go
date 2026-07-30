package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const (
	// nameResolutionPageLimit and nameResolutionPageBound bound the search for
	// the one live Sandbox holding a name. Deleted Sandboxes keep their metadata
	// and remain listable, so a long-lived name may trail historical rows.
	nameResolutionPageLimit = 200
	nameResolutionPageBound = 20
)

// resolveSandboxReference resolves an opaque Sandbox identifier or a reserved
// name. Identifiers carry a fixed prefix and a reserved name may not, so the two
// are told apart without a speculative request.
func resolveSandboxReference(
	ctx context.Context,
	client *secondboxclient.Client,
	reference string,
) (*secondboxclient.SandboxHandle, error) {
	if reference == "" {
		return nil, errors.New("SecondBox CLI requires a Sandbox name or identifier")
	}
	if strings.HasPrefix(reference, contracts.SandboxIDPrefix) {
		return resolveSandboxHandle(ctx, client, reference)
	}
	return resolveSandboxName(ctx, client, reference)
}

// resolveSandboxName finds the single live Sandbox carrying the reserved name.
//
// The reserved-name index is unique among Sandboxes that are not deleted, so at
// most one live match exists; the paging here only skips deleted predecessors.
func resolveSandboxName(
	ctx context.Context,
	client *secondboxclient.Client,
	name string,
) (*secondboxclient.SandboxHandle, error) {
	cursor := ""
	for range nameResolutionPageBound {
		query := make(url.Values)
		query.Add("metadata", contracts.SandboxNameMetadataKey+"="+name)
		query.Set("limit", strconv.Itoa(nameResolutionPageLimit))
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page secondboxclient.SandboxPage
		err := client.RequestJSON(ctx, "listSandboxes", secondboxclient.CallOptions{
			QueryParameters: query,
		}, &page)
		if err != nil {
			return nil, err
		}
		for _, sandbox := range page.Items {
			if liveSandbox(sandbox) {
				return secondboxclient.NewSandboxHandle(client, sandbox), nil
			}
		}
		if page.NextCursor == nil {
			return nil, fmt.Errorf("SecondBox CLI found no Sandbox named %q", name)
		}
		cursor = *page.NextCursor
	}
	return nil, fmt.Errorf(
		"SecondBox CLI found no live Sandbox named %q within %d pages; supply the Sandbox identifier",
		name, nameResolutionPageBound,
	)
}

func liveSandbox(sandbox secondboxclient.Sandbox) bool {
	return sandbox.DeletedAt == nil && sandbox.State != secondboxclient.SandboxStateDeleted
}
