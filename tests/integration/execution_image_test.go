package integration_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/assetcatalog"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

var integrationImagePublisher = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

func testExecutionImageAuthority(t *testing.T) *assetcatalog.ExecutionImageAuthority {
	t.Helper()
	key, err := integrationImagePublisher()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image-publisher.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	authority, err := assetcatalog.LoadExecutionImageAuthority(path, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// These lifecycle fixtures emulate materialized bytes, as they emulate compute.
// The control plane still verifies the real signature before creating assignments.
func completeTestImagePreparation(t *testing.T, pool *pgxpool.Pool, sandboxID string, now time.Time) bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT effect.id,revision.spec_json FROM secondbox.lifecycle_effects effect
	 JOIN secondbox.sandboxes sandbox ON sandbox.id=effect.sandbox_id
	 JOIN secondbox.profile_revisions revision ON revision.id=sandbox.profile_revision_id
	 WHERE effect.sandbox_id=$1 AND effect.kind='prepare_image' AND effect.state='queued'`, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	type pending struct {
		id   string
		spec contracts.ProfileRevisionSpec
	}
	var preparations []pending
	for rows.Next() {
		var value pending
		var document []byte
		if err := rows.Scan(&value.id, &document); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(document, &value.spec); err != nil {
			t.Fatal(err)
		}
		preparations = append(preparations, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, preparation := range preparations {
		runtime, err := (multirunnerAssetCatalog{}).Resolve(preparation.spec.RuntimeBundleDigest)
		if err != nil {
			t.Fatal(err)
		}
		toolchain, err := (multirunnerAssetCatalog{}).Resolve(preparation.spec.ToolchainBundleDigest)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := json.Marshal(map[string]any{"architecture": preparation.spec.Architecture, "guestProtocol": map[string]int{"minimum": 1, "maximum": 1}, "runtimeBundle": runtime, "toolchainBundle": toolchain})
		if err != nil {
			t.Fatal(err)
		}
		key, err := integrationImagePublisher()
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(manifest)
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		evidence, err := json.Marshal(&runnerv1.PrepareImageResult{OperationId: preparation.id, ResolvedDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Manifest: manifest, Signature: signature})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE secondbox.lifecycle_effects SET state='completed',evidence_json=$2 WHERE id=$1`, preparation.id, evidence); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE secondbox.runner_commands SET state='acknowledged' WHERE assignment_id=$1 AND kind='prepare-image'`, preparation.id); err != nil {
			t.Fatal(err)
		}
	}
	if len(preparations) > 0 {
		if _, err := pool.Exec(t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id=$1`, sandboxID, now); err != nil {
			t.Fatal(err)
		}
	}
	return len(preparations) > 0
}

func testExecutionImage() contracts.ExecutionImage {
	return contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
}
