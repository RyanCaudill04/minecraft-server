package deploy

// Each release also has provenance.sigstore.json, the Sigstore bundle
// actions/attest-build-provenance made for its tarballs. Install checks it
// against the checksum file before downloading the tarball, so a release
// that fails the check is never downloaded or unpacked, and the installed
// releases and current are left as they were.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// ProvenanceName is the release asset holding the Sigstore bundle that
// actions/attest-build-provenance made for the release's tarballs.
const ProvenanceName = "provenance.sigstore.json"

const maxProvenanceSize = 1 << 20

// Provenance checks that an artifact was built by the release workflow.
// *provenance.Verifier implements it.
type Provenance interface {
	// Verify checks that bundle, a Sigstore bundle, attests to an artifact
	// whose SHA-256 is digest.
	Verify(bundle []byte, digest []byte) error
}

var releaseNumber = regexp.MustCompile(`^r([0-9]{1,9})$`)

// ValidCutoff reports whether tag can be Deployer.UnattestedBefore.
func ValidCutoff(tag string) bool { return releaseNumber.MatchString(tag) }

// checkProvenance checks the release's attestation covers wantHex, the
// SHA-256 from its checksum file. It runs before the tarball is downloaded:
// the download must then match that checksum, so it is what was attested.
func (d *Deployer) checkProvenance(ctx context.Context, rel *Release, wantHex string) error {
	if d.Provenance == nil {
		return nil
	}
	bundleURL, err := rel.assetURL(ProvenanceName)
	if err != nil {
		if d.builtBeforeAttestations(rel.Tag) {
			d.Log.Printf("WARNING: installing %s without checking its provenance: it has no %s, and -unattested-before=%s allows that",
				rel.Tag, ProvenanceName, d.UnattestedBefore)
			return nil
		}
		return fmt.Errorf("deploy: refusing to install %s: it has no %s, so there is no proof the release workflow built it", rel.Tag, ProvenanceName)
	}
	digest, err := hex.DecodeString(wantHex)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := d.download(ctx, bundleURL, &b, maxProvenanceSize); err != nil {
		return err
	}
	if err := d.Provenance.Verify(b.Bytes(), digest); err != nil {
		return fmt.Errorf("deploy: refusing to install %s: %w", rel.Tag, err)
	}
	d.Log.Printf("%s provenance verified", rel.Tag)
	return nil
}

// builtBeforeAttestations reports whether tag is numbered below
// UnattestedBefore, so it may have no attestation.
func (d *Deployer) builtBeforeAttestations(tag string) bool {
	cutoff := releaseNumber.FindStringSubmatch(d.UnattestedBefore)
	m := releaseNumber.FindStringSubmatch(tag)
	if cutoff == nil || m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	c, _ := strconv.Atoi(cutoff[1])
	return n < c
}
