package portdirect

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// vectorsRelativePath reaches the single shared contract from this module.
const vectorsRelativePath = "../../../contracts/portdirect/v1/vectors.json"

type framingVectors struct {
	Magic            string `json:"magic"`
	CredentialFrames []struct {
		Credential string `json:"credential"`
		EncodedHex string `json:"encodedHex"`
	} `json:"credentialFrames"`
	VerdictFrames []struct {
		Verdict    byte   `json:"verdict"`
		Detail     string `json:"detail"`
		EncodedHex string `json:"encodedHex"`
	} `json:"verdictFrames"`
	RejectedCredentialFramesHex []string `json:"rejectedCredentialFramesHex"`
	RejectedVerdictFramesHex    []string `json:"rejectedVerdictFramesHex"`
}

func loadFramingVectors(t *testing.T) framingVectors {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(vectorsRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	var vectors framingVectors
	if err := json.Unmarshal(content, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Magic != Magic {
		t.Fatalf("magic = %q, want %q", vectors.Magic, Magic)
	}
	return vectors
}

// TestFramingMatchesSharedVectors pins this implementation to the shared wire
// contract. The runner module carries a deliberately separate copy of this
// package so it stays dependency-free; both assert against the same file, so a
// change to one that is not mirrored fails here rather than corrupting a
// handshake at runtime.
func TestFramingMatchesSharedVectors(t *testing.T) {
	vectors := loadFramingVectors(t)

	for _, vector := range vectors.CredentialFrames {
		expected, err := hex.DecodeString(vector.EncodedHex)
		if err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := WriteCredential(&encoded, vector.Credential); err != nil {
			t.Fatalf("WriteCredential(%d bytes): %v", len(vector.Credential), err)
		}
		if !bytes.Equal(encoded.Bytes(), expected) {
			t.Errorf("credential frame for %d bytes = %x, want %x",
				len(vector.Credential), encoded.Bytes(), expected)
		}
		decoded, err := ReadCredential(bytes.NewReader(expected))
		if err != nil {
			t.Fatalf("ReadCredential: %v", err)
		}
		if decoded != vector.Credential {
			t.Errorf("decoded credential = %d bytes, want %d", len(decoded), len(vector.Credential))
		}
	}

	for _, vector := range vectors.VerdictFrames {
		expected, err := hex.DecodeString(vector.EncodedHex)
		if err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := WriteVerdict(&encoded, Verdict(vector.Verdict), vector.Detail); err != nil {
			t.Fatalf("WriteVerdict: %v", err)
		}
		if !bytes.Equal(encoded.Bytes(), expected) {
			t.Errorf("verdict frame %d/%q = %x, want %x",
				vector.Verdict, vector.Detail, encoded.Bytes(), expected)
		}
		verdict, detail, err := ReadVerdict(bytes.NewReader(expected))
		if err != nil {
			t.Fatalf("ReadVerdict: %v", err)
		}
		if verdict != Verdict(vector.Verdict) || detail != vector.Detail {
			t.Errorf("decoded verdict = %d/%q, want %d/%q",
				verdict, detail, vector.Verdict, vector.Detail)
		}
	}
}

// TestFramingRejectsSharedMalformedVectors proves a peer that is not speaking
// this protocol is denied rather than partially accepted.
func TestFramingRejectsSharedMalformedVectors(t *testing.T) {
	vectors := loadFramingVectors(t)
	for index, encodedHex := range vectors.RejectedCredentialFramesHex {
		encoded, err := hex.DecodeString(encodedHex)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCredential(bytes.NewReader(encoded)); err == nil {
			t.Errorf("rejected credential vector %d was accepted", index)
		}
	}
	for index, encodedHex := range vectors.RejectedVerdictFramesHex {
		encoded, err := hex.DecodeString(encodedHex)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadVerdict(bytes.NewReader(encoded)); err == nil {
			t.Errorf("rejected verdict vector %d was accepted", index)
		}
	}
}
