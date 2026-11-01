package provenance

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The testdata is real and recorded, so verification runs offline:
//
//   - cli-gh-linux-arm64.sigstore.json is the build provenance GitHub's own
//     release workflow made for gh_2.103.3_linux_arm64.tar.gz, from
//     "gh attestation download -R cli/cli". It was signed by
//     .github/workflows/deployment.yml on refs/heads/trunk of cli/cli.
//   - trusted_root.json is the Sigstore public-good trusted root, from
//     "gh attestation trusted-root".
const ghArm64 = "ee32df2323c8847f5fac6cb6b4b69ca75828e9329a8f22f677021f7b3306ce63"

func load(t *testing.T) ([]byte, root.TrustedMaterial) {
	t.Helper()
	b, err := os.ReadFile("testdata/cli-gh-linux-arm64.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	return b, tr
}

func digest(t *testing.T, s string) []byte {
	t.Helper()
	d, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ghVerifier(tr root.TrustedMaterial) *Verifier {
	return &Verifier{Repo: "cli/cli", Workflow: ".github/workflows/deployment.yml", Ref: "refs/heads/trunk", Trusted: tr}
}

// tamper rewrites the signed statement's first subject digest, as someone
// swapping in their own tarball would, without re-signing it.
func tamper(t *testing.T, bundleJSON []byte, newDigest string) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatal(err)
	}
	env := b["dsseEnvelope"].(map[string]any)
	payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	st["subject"].([]any)[0].(map[string]any)["digest"] = map[string]any{"sha256": newDigest}
	payload, _ = json.Marshal(st)
	env["payload"] = base64.StdEncoding.EncodeToString(payload)
	out, _ := json.Marshal(b)
	return out
}

func TestVerify(t *testing.T) {
	bundleJSON, tr := load(t)
	evil := strings.Repeat("ab", 32)

	tests := []struct {
		name    string
		v       *Verifier
		bundle  []byte
		digest  string
		wantErr string // "" means it must verify
	}{
		{name: "valid", v: ghVerifier(tr), bundle: bundleJSON, digest: ghArm64},
		{name: "repo matches ignoring case", v: func() *Verifier {
			v := ghVerifier(tr)
			v.Repo = "CLI/Cli"
			return v
		}(), bundle: bundleJSON, digest: ghArm64},
		{name: "wrong repo", v: func() *Verifier {
			v := ghVerifier(tr)
			v.Repo = "RyanCaudill04/minecraft-server"
			return v
		}(), bundle: bundleJSON, digest: ghArm64, wantErr: "expected SAN value"},
		{name: "repo prefix", v: func() *Verifier {
			v := ghVerifier(tr)
			v.Repo = "cli/cl"
			return v
		}(), bundle: bundleJSON, digest: ghArm64, wantErr: "expected SAN value"},
		{name: "wrong workflow", v: func() *Verifier {
			v := ghVerifier(tr)
			v.Workflow = ".github/workflows/ci.yml"
			return v
		}(), bundle: bundleJSON, digest: ghArm64, wantErr: "expected SAN value"},
		{name: "wrong ref", v: func() *Verifier {
			v := ghVerifier(tr)
			v.Ref = "refs/heads/main"
			return v
		}(), bundle: bundleJSON, digest: ghArm64, wantErr: "expected SAN value"},
		{name: "tampered artifact", v: ghVerifier(tr), bundle: bundleJSON, digest: evil, wantErr: "digest"},
		{name: "tampered statement", v: ghVerifier(tr), bundle: tamper(t, bundleJSON, evil), digest: evil, wantErr: "does not match envelope payload hash"},
		{name: "not a bundle", v: ghVerifier(tr), bundle: []byte(`{"hello": "world"}`), digest: ghArm64, wantErr: "reading bundle"},
		{name: "unset identity", v: &Verifier{Trusted: tr}, bundle: bundleJSON, digest: ghArm64, wantErr: "must be set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.v.Verify(bytes.Clone(tt.bundle), digest(t, tt.digest))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Verify: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Verify succeeded, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Verify error %q doesn't mention %q", err, tt.wantErr)
			}
		})
	}
}
