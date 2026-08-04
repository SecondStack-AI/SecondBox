// Package resourceapply checks and converges declarative SecondBox resources.
package resourceapply

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

const SchemaVersion = "secondbox.resources/v1"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Document is a non-secret desired-state document. Absence never means delete.
type Document struct {
	SchemaVersion string       `json:"schemaVersion"`
	RunnerPools   []RunnerPool `json:"runnerPools"`
	Profiles      []Profile    `json:"profiles"`
}

type RunnerPool struct {
	Name           string                                 `json:"name"`
	Architectures  secondboxclient.RunnerArchitectureList `json:"architectures"`
	Capabilities   secondboxclient.RunnerCapabilityList   `json:"capabilities"`
	CapacityPolicy secondboxclient.RunnerCapacityPolicy   `json:"capacityPolicy"`
	State          secondboxclient.RunnerPoolState        `json:"state"`
	MutableFields  []string                               `json:"mutableFields"`
}

type Profile struct {
	Name      string            `json:"name"`
	Revisions []ProfileRevision `json:"revisions"`
}

type ProfileRevision struct {
	Number     int64                               `json:"number"`
	SpecDigest string                              `json:"specDigest"`
	Spec       secondboxclient.ProfileRevisionSpec `json:"spec"`
}

type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionAppend Action = "append"
	ActionNoop   Action = "noop"
)

type Result struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Action       Action `json:"action"`
	FromRevision int64  `json:"fromRevision,omitempty"`
	ToRevision   int64  `json:"toRevision,omitempty"`
}

type Report struct {
	SchemaVersion string   `json:"schemaVersion"`
	Applied       bool     `json:"applied"`
	Results       []Result `json:"results"`
}

// Client is the high-level Go SDK surface used by both CLI and deployment tooling.
type Client interface {
	GetRunnerPool(context.Context, secondboxclient.ProfileName) (secondboxclient.RunnerPool, error)
	CreateRunnerPool(context.Context, secondboxclient.CreateRunnerPoolRequest) (secondboxclient.RunnerPool, error)
	UpdateRunnerPool(context.Context, secondboxclient.ProfileName, int64, secondboxclient.UpdateRunnerPoolRequest) (secondboxclient.RunnerPool, error)
	GetProfile(context.Context, secondboxclient.ProfileName) (secondboxclient.Profile, error)
	CreateProfile(context.Context, secondboxclient.CreateProfileRequest, string) (secondboxclient.Profile, error)
	ReviseProfile(context.Context, secondboxclient.ProfileName, int64, secondboxclient.ReviseProfileRequest, string) (secondboxclient.Profile, error)
}

func Decode(data []byte) (Document, error) {
	var document Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("SecondBox resource document decode failed: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("SecondBox resource document must contain one JSON value")
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func Encode(document Document) ([]byte, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(document, "", "  ")
}

func SpecDigest(spec secondboxclient.ProfileRevisionSpec) (string, error) {
	canonical, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("SecondBox Profile spec canonicalization failed: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (document Document) Validate() error {
	if document.SchemaVersion != SchemaVersion {
		return fmt.Errorf("SecondBox resource schemaVersion must be %q", SchemaVersion)
	}
	pools := make(map[string]struct{}, len(document.RunnerPools))
	for index, pool := range document.RunnerPools {
		if pool.Name == "" || len(pool.Architectures) == 0 || pool.State == "" || pool.CapacityPolicy == nil {
			return fmt.Errorf("SecondBox RunnerPool %d is incomplete", index)
		}
		if _, duplicate := pools[pool.Name]; duplicate {
			return fmt.Errorf("SecondBox RunnerPool %q is duplicated", pool.Name)
		}
		pools[pool.Name] = struct{}{}
		seenMutable := map[string]bool{}
		for _, field := range pool.MutableFields {
			if field != "state" && field != "capacityPolicy" {
				return fmt.Errorf("SecondBox RunnerPool %q mutable field %q is unsupported", pool.Name, field)
			}
			if seenMutable[field] {
				return fmt.Errorf("SecondBox RunnerPool %q mutable field %q is duplicated", pool.Name, field)
			}
			seenMutable[field] = true
		}
	}
	profiles := make(map[string]struct{}, len(document.Profiles))
	for _, profile := range document.Profiles {
		if profile.Name == "" || len(profile.Revisions) == 0 {
			return fmt.Errorf("SecondBox Profile %q requires a complete revision lineage", profile.Name)
		}
		if _, duplicate := profiles[profile.Name]; duplicate {
			return fmt.Errorf("SecondBox Profile %q is duplicated", profile.Name)
		}
		profiles[profile.Name] = struct{}{}
		for index, revision := range profile.Revisions {
			wantNumber := int64(index + 1)
			if revision.Number != wantNumber {
				return fmt.Errorf("SecondBox Profile %q revision lineage has gap at %d", profile.Name, wantNumber)
			}
			if !digestPattern.MatchString(revision.SpecDigest) {
				return fmt.Errorf("SecondBox Profile %q revision %d specDigest is not canonical sha256", profile.Name, revision.Number)
			}
			actual, err := SpecDigest(revision.Spec)
			if err != nil || actual != revision.SpecDigest {
				return fmt.Errorf("SecondBox Profile %q revision %d spec digest mismatch", profile.Name, revision.Number)
			}
			if _, ok := pools[revision.Spec.Pool]; !ok {
				return fmt.Errorf("SecondBox Profile %q revision %d references undeclared RunnerPool %q", profile.Name, revision.Number, revision.Spec.Pool)
			}
		}
	}
	return nil
}

func Check(ctx context.Context, client Client, document Document) (Report, error) {
	return converge(ctx, client, document, false)
}

func Apply(ctx context.Context, client Client, document Document) (Report, error) {
	return converge(ctx, client, document, true)
}

func converge(ctx context.Context, client Client, document Document, apply bool) (Report, error) {
	if client == nil {
		return Report{}, errors.New("SecondBox resource client is required")
	}
	if err := document.Validate(); err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: SchemaVersion, Applied: apply, Results: []Result{}}
	documentDigest, err := documentIdentity(document)
	if err != nil {
		return Report{}, err
	}
	pools := slices.Clone(document.RunnerPools)
	slices.SortFunc(pools, func(a, b RunnerPool) int { return strings.Compare(a.Name, b.Name) })
	for _, desired := range pools {
		result, err := convergePool(ctx, client, desired, apply)
		if err != nil {
			return report, err
		}
		report.Results = append(report.Results, result)
	}
	profiles := slices.Clone(document.Profiles)
	slices.SortFunc(profiles, func(a, b Profile) int { return strings.Compare(a.Name, b.Name) })
	for _, desired := range profiles {
		results, err := convergeProfile(ctx, client, desired, apply, documentDigest)
		if err != nil {
			return report, err
		}
		report.Results = append(report.Results, results...)
	}
	return report, nil
}

func convergePool(ctx context.Context, client Client, desired RunnerPool, apply bool) (Result, error) {
	current, err := client.GetRunnerPool(ctx, desired.Name)
	if isNotFound(err) {
		result := Result{Kind: "runnerPool", Name: desired.Name, Action: ActionCreate, ToRevision: 1}
		if !apply {
			return result, nil
		}
		_, err = client.CreateRunnerPool(ctx, secondboxclient.CreateRunnerPoolRequest{Name: desired.Name, Architectures: desired.Architectures, Capabilities: desired.Capabilities, CapacityPolicy: desired.CapacityPolicy, State: desired.State})
		return result, err
	}
	if err != nil {
		return Result{}, fmt.Errorf("SecondBox resource RunnerPool %q lookup failed: %w", desired.Name, err)
	}
	if !equalStrings(current.Architectures, desired.Architectures) || !equalStrings(current.Capabilities, desired.Capabilities) {
		return Result{}, fmt.Errorf("SecondBox RunnerPool %q has incompatible architecture or capability drift", desired.Name)
	}
	update := secondboxclient.UpdateRunnerPoolRequest{}
	changed := false
	if current.State != desired.State {
		if !slices.Contains(desired.MutableFields, "state") {
			return Result{}, fmt.Errorf("SecondBox RunnerPool %q state drift is not mutable", desired.Name)
		}
		update.State = &desired.State
		changed = true
	}
	if !reflect.DeepEqual(current.CapacityPolicy, desired.CapacityPolicy) {
		if !slices.Contains(desired.MutableFields, "capacityPolicy") {
			return Result{}, fmt.Errorf("SecondBox RunnerPool %q capacityPolicy drift is not mutable", desired.Name)
		}
		update.CapacityPolicy = desired.CapacityPolicy
		changed = true
	}
	if !changed {
		return Result{Kind: "runnerPool", Name: desired.Name, Action: ActionNoop, FromRevision: current.Revision, ToRevision: current.Revision}, nil
	}
	result := Result{Kind: "runnerPool", Name: desired.Name, Action: ActionUpdate, FromRevision: current.Revision, ToRevision: current.Revision + 1}
	if apply {
		_, err = client.UpdateRunnerPool(ctx, desired.Name, current.Revision, update)
	}
	return result, err
}

func equalStrings[Slice ~[]string](left Slice, right Slice) bool {
	leftCanonical := slices.Clone(left)
	rightCanonical := slices.Clone(right)
	slices.Sort(leftCanonical)
	slices.Sort(rightCanonical)
	return slices.Equal(leftCanonical, rightCanonical)
}

func convergeProfile(ctx context.Context, client Client, desired Profile, apply bool, documentDigest string) ([]Result, error) {
	results := []Result{}
	current, err := client.GetProfile(ctx, desired.Name)
	if isNotFound(err) {
		results = append(results, Result{Kind: "profile", Name: desired.Name, Action: ActionCreate, ToRevision: 1})
		if !apply {
			for _, revision := range desired.Revisions[1:] {
				results = append(results, Result{Kind: "profile", Name: desired.Name, Action: ActionAppend, FromRevision: revision.Number - 1, ToRevision: revision.Number})
			}
			return results, nil
		}
		created, createErr := client.CreateProfile(ctx, secondboxclient.CreateProfileRequest{Name: desired.Name, Spec: desired.Revisions[0].Spec}, idempotencyKey(documentDigest, desired.Name, 1))
		if createErr != nil {
			return nil, createErr
		}
		current = created
	} else if err != nil {
		return nil, fmt.Errorf("SecondBox resource Profile %q lookup failed: %w", desired.Name, err)
	} else {
		if current.State != secondboxclient.ProfileStateEnabled {
			return nil, fmt.Errorf("SecondBox Profile %q is disabled", desired.Name)
		}
		if len(current.Revisions) == 0 {
			return nil, fmt.Errorf("SecondBox Profile %q returned no immutable lineage", desired.Name)
		}
		if len(current.Revisions) > len(desired.Revisions) {
			return nil, fmt.Errorf("SecondBox Profile %q has unknown future head revision %d", desired.Name, current.CurrentRevision.Number)
		}
		for index, installed := range current.Revisions {
			if installed.Number != int64(index+1) {
				return nil, fmt.Errorf("SecondBox Profile %q installed lineage has gap at revision %d", desired.Name, index+1)
			}
			digest, digestErr := SpecDigest(installed.Spec)
			if digestErr != nil || digest != desired.Revisions[index].SpecDigest {
				return nil, fmt.Errorf("SecondBox Profile %q historical revision %d was altered", desired.Name, installed.Number)
			}
		}
		if current.CurrentRevision.Number != int64(len(current.Revisions)) {
			return nil, fmt.Errorf("SecondBox Profile %q current head is incompatible with its lineage", desired.Name)
		}
	}
	if current.CurrentRevision.Number == int64(len(desired.Revisions)) {
		if len(results) != 0 {
			return results, nil
		}
		return append(results, Result{Kind: "profile", Name: desired.Name, Action: ActionNoop, FromRevision: current.Revision, ToRevision: current.Revision}), nil
	}
	for index := current.CurrentRevision.Number; index < int64(len(desired.Revisions)); index++ {
		revision := desired.Revisions[index]
		result := Result{Kind: "profile", Name: desired.Name, Action: ActionAppend, FromRevision: current.Revision, ToRevision: current.Revision + 1}
		if apply {
			current, err = client.ReviseProfile(ctx, desired.Name, current.Revision, secondboxclient.ReviseProfileRequest{Spec: revision.Spec}, idempotencyKey(documentDigest, desired.Name, revision.Number))
			if err != nil {
				return results, err
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func documentIdentity(document Document) (string, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("SecondBox resource document identity failed: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func idempotencyKey(documentDigest, name string, revision int64) string {
	return fmt.Sprintf("resource-apply:%s:%s:%d", documentDigest, name, revision)
}

func isNotFound(err error) bool {
	var failure *secondboxclient.APIError
	return errors.As(err, &failure) && failure.StatusCode == http.StatusNotFound
}
