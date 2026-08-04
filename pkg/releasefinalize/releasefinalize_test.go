package releasefinalize

import (
	"strings"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/releasecontract"
)

func TestQualificationRejectsIncompleteRunnerEnvironment(t *testing.T) {
	suiteDigest := "sha256:" + strings.Repeat("e", 64)
	manifest := releasecontract.ArtifactManifest{Identity: releasecontract.Identity{Version: "1.2.3", Tag: "v1.2.3", SourceCommit: strings.Repeat("a", 40)}, RunnerProtocol: releasecontract.ProtocolWindow{Maximum: 1}, GuestProtocol: releasecontract.ProtocolWindow{Maximum: 1}, Runner: releasecontract.OCIArtifact{Reference: releasecontract.RunnerImage + "@sha256:" + strings.Repeat("b", 64)}, MicroVM: releasecontract.MicroVMArtifact{SignedManifestDigest: "sha256:" + strings.Repeat("c", 64), SigningKeyFingerprint: "SHA256:" + strings.Repeat("D", 64)}, SourceFreeSuite: releasecontract.Reference{Digest: suiteDigest}}
	if _, err := Qualification(manifest, []byte("manifest"), QualificationInput{SuiteDigest: suiteDigest}); err == nil {
		t.Fatal("incomplete qualification input was accepted")
	}
}
