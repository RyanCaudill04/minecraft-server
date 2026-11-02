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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ryancaudill04/minecraft-server/web/internal/provenance"
)

// ProvenanceName is the release asset holding the Sigstore bundle that
// actions/attest-build-provenance made for the release's tarballs.
const ProvenanceName = "provenance.sigstore.json"

const maxProvenanceSize = 1 << 20

// refused marks an error that retrying can't fix: the release itself is
// wrong, so Run records it as failed instead of trying it every run.
type refused struct{ error }

func (r refused) Unwrap() error { return r.error }

// Provenance checks that an artifact was built by the release workflow.
// *provenance.Verifier implements it.
type Provenance interface {
	// Verify checks that bundle, a Sigstore bundle, attests to an artifact
	// called name whose SHA-256 is digest.
	Verify(ctx context.Context, bundle []byte, digest []byte, name string) error
}

var releaseNumber = regexp.MustCompile(`^r([0-9]{1,9})$`)

// ValidCutoff reports whether tag can be Deployer.UnattestedBefore.
func ValidCutoff(tag string) bool { return releaseNumber.MatchString(tag) }

// checkProvenance checks the release's attestation covers wantHex, the
// SHA-256 from its checksum file, and reports whether it did: false means
// the release is old enough to have none. It runs before the tarball is downloaded:
// the download must then match that checksum, so it is what was attested.
func (d *Deployer) checkProvenance(ctx context.Context, rel *Release, wantHex string) (bool, error) {
	if d.Provenance == nil {
		return false, nil
	}
	bundleURL, err := rel.assetURL(ProvenanceName)
	if err != nil {
		if d.builtBeforeAttestations(rel.Tag) {
			d.Log.Printf("WARNING: installing %s without checking its provenance: it has no %s, and -unattested-before=%s allows that",
				rel.Tag, ProvenanceName, d.UnattestedBefore)
			return false, nil
		}
		return false, refused{fmt.Errorf("deploy: refusing to install %s: it has no %s, so there is no proof the release workflow built it", rel.Tag, ProvenanceName)}
	}
	digest, err := hex.DecodeString(wantHex)
	if err != nil {
		return false, err
	}
	var b bytes.Buffer
	if err := d.download(ctx, bundleURL, &b, maxProvenanceSize); err != nil {
		return false, err
	}
	if err := d.Provenance.Verify(ctx, b.Bytes(), digest, TarballName); err != nil {
		err = fmt.Errorf("deploy: refusing to install %s: %w", rel.Tag, err)
		if errors.Is(err, provenance.ErrUnavailable) {
			return false, err // the trust root, not the release; try again
		}
		return false, refused{err}
	}
	d.Log.Printf("%s provenance verified", rel.Tag)
	return true, nil
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

// checkVersion ties the unpacked release to its tag. The attestation only
// says the release workflow built these bytes at some point; the VERSION
// file inside them, written by build-release.sh, says for which tag, so an
// old attested tarball can't be published under a newer tag.
func checkVersion(dir, tag string) error {
	data, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return refused{fmt.Errorf("deploy: refusing to install %s: its attested tarball has no VERSION: %w", tag, err)}
	}
	if got := strings.TrimSpace(string(data)); got != tag {
		return refused{fmt.Errorf("deploy: refusing to install %s: the attested tarball is for %q", tag, got)}
	}
	return nil
}

// olderThan reports whether tag and current are both numbered releases and
// tag comes before current.
func olderThan(tag, current string) bool {
	t := releaseNumber.FindStringSubmatch(tag)
	c := releaseNumber.FindStringSubmatch(current)
	if t == nil || c == nil {
		return false
	}
	tn, _ := strconv.Atoi(t[1])
	cn, _ := strconv.Atoi(c[1])
	return tn < cn
}
