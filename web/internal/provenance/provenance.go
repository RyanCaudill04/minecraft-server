// Package provenance checks that a release was built by this repo's
// release workflow, not just uploaded by someone who can publish releases.
//
// The release job signs its tarballs with actions/attest-build-provenance,
// which writes a Sigstore bundle: an in-toto SLSA provenance statement
// naming each tarball's SHA-256, signed with a short-lived Fulcio
// certificate issued to the workflow run, and logged in Rekor. Verify
// checks that bundle against the Sigstore public-good trusted root, then
// checks that the certificate was issued to the expected workflow file, on
// the expected ref of the expected repo, on a GitHub-hosted runner.
package provenance

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Issuer is the OIDC issuer of GitHub Actions' workflow tokens.
const Issuer = "https://token.actions.githubusercontent.com"

// SLSAProvenance is the predicate type actions/attest-build-provenance signs.
const SLSAProvenance = "https://slsa.dev/provenance/v1"

// Verifier checks Sigstore bundles made by actions/attest-build-provenance.
type Verifier struct {
	Repo     string // GitHub "owner/name"; compared ignoring case, like GitHub does
	Workflow string // workflow file that must have signed, e.g. .github/workflows/ci.yml
	Ref      string // ref it must have run on, e.g. refs/heads/main

	// Trusted is the Sigstore trusted root. Nil fetches the public-good
	// root through TUF on each Verify, caching its metadata in CacheDir
	// (or ~/.sigstore/root if that is empty).
	Trusted  root.TrustedMaterial
	CacheDir string
}

// Verify checks that bundleJSON, a Sigstore bundle, is valid, was signed
// by v's workflow, and attests to an artifact whose SHA-256 is digest.
func (v *Verifier) Verify(bundleJSON []byte, digest []byte) error {
	if v.Repo == "" || v.Workflow == "" || v.Ref == "" {
		return errors.New("provenance: Repo, Workflow and Ref must be set")
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("provenance: reading bundle: %w", err)
	}

	trusted := v.Trusted
	if trusted == nil {
		opts := tuf.DefaultOptions()
		if v.CacheDir != "" {
			opts = opts.WithCachePath(v.CacheDir)
		}
		tr, err := root.FetchTrustedRootWithOptions(opts)
		if err != nil {
			return fmt.Errorf("provenance: fetching the Sigstore trusted root: %w", err)
		}
		trusted = tr
	}

	// The same checks as "gh attestation verify" for a public repo: an SCT
	// from the CT log, an entry in Rekor, and a time from Rekor at which
	// the certificate was valid.
	sv, err := verify.NewVerifier(trusted,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	id, err := v.identity()
	if err != nil {
		return err
	}
	res, err := sv.Verify(&b, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest),
		verify.WithCertificateIdentity(id)))
	if err != nil {
		return fmt.Errorf("provenance: artifact sha256:%s: %w", hex.EncodeToString(digest), err)
	}
	if res.Statement == nil || res.Statement.PredicateType != SLSAProvenance {
		got := "none"
		if res.Statement != nil {
			got = res.Statement.PredicateType
		}
		return fmt.Errorf("provenance: statement is %s, want %s", got, SLSAProvenance)
	}
	return nil
}

// identity is the certificate v's workflow gets: its SAN is the workflow
// file's URL at the ref it ran on. Checking the source ref as well rejects
// a run of another ref that calls this workflow as a reusable one.
func (v *Verifier) identity() (verify.CertificateIdentity, error) {
	san := "^https://github\\.com/(?i:" + regexp.QuoteMeta(v.Repo) + ")/" +
		regexp.QuoteMeta(v.Workflow) + "@" + regexp.QuoteMeta(v.Ref) + "$"
	sanMatcher, err := verify.NewSANMatcher("", san)
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("provenance: %w", err)
	}
	issuer, err := verify.NewIssuerMatcher(Issuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("provenance: %w", err)
	}
	id, err := verify.NewCertificateIdentity(sanMatcher, issuer, certificate.Extensions{
		SourceRepositoryRef: v.Ref,
		RunnerEnvironment:   "github-hosted",
	})
	if err != nil {
		return verify.CertificateIdentity{}, fmt.Errorf("provenance: %w", err)
	}
	return id, nil
}
