// Package plugins reads the Paper plugins installed in a server's plugins
// folder and checks them against the pinned manifest (plugins/manifest.json):
// each jar's SHA-256 and the name and version from its plugin.yml or
// paper-plugin.yml.
//
// It only reads. It never extracts or runs anything from a jar: it opens
// the jar as a zip and reads one small metadata file. The same checks are
// what an install from admin (#76) has to pass before a jar is staged.
package plugins

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// MaxJarSize is the largest jar this package reads. The plugins here are
// 1 to 8 MB; anything past 50 MB is refused rather than hashed.
const MaxJarSize = 50 << 20

// maxDescriptor caps how much of plugin.yml is read, so a jar can't make
// us inflate a huge file.
const maxDescriptor = 64 << 10

var (
	ErrTooLarge  = errors.New("larger than 50 MB")
	ErrNotJar    = errors.New("not a jar (zip) file")
	ErrMod       = errors.New("a Fabric, Forge or NeoForge mod, not a Paper plugin; Paper can't load mods")
	ErrNoPlugin  = errors.New("no plugin.yml or paper-plugin.yml")
	ErrBadPlugin = errors.New("plugin.yml is missing name, version or main")
)

// Meta is what a jar's plugin.yml (or paper-plugin.yml) says about it.
type Meta struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Main       string `json:"main"`
	APIVersion string `json:"api_version,omitzero"`
	Paper      bool   `json:"paper,omitzero"` // from paper-plugin.yml
}

// modFiles mark a jar as a mod for another loader.
var modFiles = []string{"fabric.mod.json", "quilt.mod.json", "META-INF/mods.toml", "META-INF/neoforge.mods.toml"}

// ReadMeta reads the plugin metadata from a jar of the given size. It
// refuses mods and jars without a usable plugin.yml.
func ReadMeta(r io.ReaderAt, size int64) (*Meta, error) {
	if size > MaxJarSize {
		return nil, ErrTooLarge
	}
	z, err := zip.NewReader(r, size)
	if err != nil {
		return nil, ErrNotJar
	}
	files := map[string]*zip.File{}
	for _, f := range z.File {
		files[f.Name] = f
	}
	// Paper prefers paper-plugin.yml when a jar has both.
	f, paper := files["paper-plugin.yml"], true
	if f == nil {
		f, paper = files["plugin.yml"], false
	}
	if f == nil {
		// Some jars (ViaVersion) run on several loaders and hold both a
		// plugin.yml and mod metadata, so a jar is only a mod if it has
		// no plugin.yml.
		for _, name := range modFiles {
			if files[name] != nil {
				return nil, ErrMod
			}
		}
		return nil, ErrNoPlugin
	}
	if f.UncompressedSize64 > maxDescriptor {
		return nil, fmt.Errorf("%s is larger than 64 KB", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxDescriptor+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.Name, err)
	}
	if len(data) > maxDescriptor {
		return nil, fmt.Errorf("%s is larger than 64 KB", f.Name)
	}
	m := parseDescriptor(data)
	m.Paper = paper
	if m.Name == "" || m.Version == "" || m.Main == "" {
		return nil, ErrBadPlugin
	}
	return m, nil
}

// topLevel matches a top-level "key: value" line of plugin.yml.
var topLevel = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):[ \t]*(.*)$`)

// parseDescriptor reads the top-level scalar keys this package needs. The
// four it uses are always plain one-line scalars, so this avoids taking a
// YAML library as a dependency; anything nested or multi-line is skipped.
func parseDescriptor(data []byte) *Meta {
	m := &Meta{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), maxDescriptor)
	for sc.Scan() {
		match := topLevel.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r"))
		if match == nil {
			continue
		}
		v := scalar(match[2])
		switch match[1] {
		case "name":
			m.Name = v
		case "version":
			m.Version = v
		case "main":
			m.Main = v
		case "api-version":
			m.APIVersion = v
		}
	}
	return m
}

// scalar unquotes a one-line YAML scalar and drops a trailing comment.
func scalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : end+1]
		}
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "|" || s == ">" || strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*") {
		return ""
	}
	return s
}

// Entry is one plugin in the manifest: the exact jar the repo expects,
// and where it came from.
type Entry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Source  string `json:"source"`
}

// Manifest is plugins/manifest.json.
type Manifest struct {
	Plugins []Entry `json:"plugins"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadManifest reads and checks a manifest. A missing file is not an
// error: it returns nil, and every installed jar shows as unpinned.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, p := range m.Plugins {
		switch {
		case p.Name == "" || p.Version == "" || p.Source == "":
			return nil, fmt.Errorf("%s: %q needs name, version and source", path, p.File)
		case p.File != filepath.Base(p.File) || !strings.HasSuffix(p.File, ".jar"):
			return nil, fmt.Errorf("%s: %q must be a jar file name with no folder", path, p.File)
		case !sha256Hex.MatchString(p.SHA256):
			return nil, fmt.Errorf("%s: %q has a bad sha256", path, p.File)
		case !strings.HasPrefix(p.Source, "https://"):
			return nil, fmt.Errorf("%s: %q source must be https", path, p.File)
		case seen[p.File]:
			return nil, fmt.Errorf("%s: %q is listed twice", path, p.File)
		}
		seen[p.File] = true
	}
	return &m, nil
}

// Status says how an installed jar compares with the manifest.
type Status string

const (
	StatusPinned   Status = "pinned"   // in the manifest with the same SHA-256
	StatusChanged  Status = "changed"  // in the manifest, but the SHA-256 differs
	StatusUnpinned Status = "unpinned" // not in the manifest
	StatusInvalid  Status = "invalid"  // not a plugin Paper can load (see Problem)
	StatusMissing  Status = "missing"  // in the manifest, but not installed
)

// Plugin is one jar in the plugins folder, or one the manifest expects.
type Plugin struct {
	File    string `json:"file"`
	Size    int64  `json:"size,omitzero"`
	SHA256  string `json:"sha256,omitzero"`
	Meta    *Meta  `json:"meta,omitzero"`
	Status  Status `json:"status"`
	Problem string `json:"problem,omitzero"`
	Source  string `json:"source,omitzero"` // from the manifest
}

// List reads every jar directly in dir (Paper ignores subfolders) and
// compares it with m, which may be nil. Jars the manifest names that
// aren't installed are listed as missing. The result is sorted by file.
func List(dir string, m *Manifest) ([]Plugin, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pinned := map[string]Entry{}
	if m != nil {
		for _, p := range m.Plugins {
			pinned[p.File] = p
		}
	}
	var out []Plugin
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jar") {
			continue
		}
		p := inspect(filepath.Join(dir, e.Name()))
		if want, ok := pinned[p.File]; ok {
			p.Source = want.Source
			delete(pinned, p.File)
			if p.Status == "" {
				p.Status = StatusChanged
				if p.SHA256 == want.SHA256 {
					p.Status = StatusPinned
				}
			}
		} else if p.Status == "" {
			p.Status = StatusUnpinned
		}
		out = append(out, p)
	}
	for _, want := range pinned {
		out = append(out, Plugin{
			File:   want.File,
			SHA256: want.SHA256,
			Meta:   &Meta{Name: want.Name, Version: want.Version},
			Status: StatusMissing,
			Source: want.Source,
		})
	}
	slices.SortFunc(out, func(a, b Plugin) int { return strings.Compare(a.File, b.File) })
	return out, nil
}

// inspect hashes one jar and reads its metadata. Status is left empty
// unless the jar is invalid.
func inspect(path string) Plugin {
	p := Plugin{File: filepath.Base(path)}
	f, err := os.Open(path)
	if err != nil {
		return invalid(p, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return invalid(p, err)
	}
	p.Size = info.Size()
	if p.Size > MaxJarSize {
		return invalid(p, ErrTooLarge)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, MaxJarSize+1)); err != nil {
		return invalid(p, err)
	}
	p.SHA256 = hex.EncodeToString(h.Sum(nil))
	meta, err := ReadMeta(f, p.Size)
	if err != nil {
		return invalid(p, err)
	}
	p.Meta = meta
	return p
}

func invalid(p Plugin, err error) Plugin {
	p.Status, p.Problem = StatusInvalid, err.Error()
	return p
}
