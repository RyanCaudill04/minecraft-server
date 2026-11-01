package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jar builds a zip holding files (name to contents).
func jar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func meta(t *testing.T, b []byte) (*Meta, error) {
	t.Helper()
	return ReadMeta(bytes.NewReader(b), int64(len(b)))
}

const squaremapYML = `api-version: '26.2'
name: squaremap
version: 1.3.15
main: xyz.jpenilla.squaremap.paper.SquaremapPaperBootstrap
description: Minimalistic and lightweight world map viewer for Minecraft servers
load: STARTUP
authors:
- jmp
website: https://github.com/jpenilla/squaremap/
folia-supported: true
`

func TestReadMetaPluginYML(t *testing.T) {
	m, err := meta(t, jar(t, map[string]string{"plugin.yml": squaremapYML, "a/B.class": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{Name: "squaremap", Version: "1.3.15", Main: "xyz.jpenilla.squaremap.paper.SquaremapPaperBootstrap", APIVersion: "26.2"}
	if *m != want {
		t.Errorf("got %+v, want %+v", *m, want)
	}
}

func TestReadMetaPaperPluginYML(t *testing.T) {
	m, err := meta(t, jar(t, map[string]string{
		"plugin.yml":       "name: Old\nversion: 1\nmain: a.Old\n",
		"paper-plugin.yml": "name: \"New\"  # quoted\nversion: 2.0 # comment\nmain: a.New\r\napi-version: \"26.2\"\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{Name: "New", Version: "2.0", Main: "a.New", APIVersion: "26.2", Paper: true}
	if *m != want {
		t.Errorf("got %+v, want %+v", *m, want)
	}
}

// A jar built for several loaders, like ViaVersion's, is still a plugin.
func TestReadMetaMultiLoaderJar(t *testing.T) {
	m, err := meta(t, jar(t, map[string]string{
		"plugin.yml":           "name: ViaVersion\nmain: com.viaversion.viaversion.ViaVersionPlugin\nversion: \"5.12.0\"\n",
		"fabric.mod.json":      "{}",
		"velocity-plugin.json": "{}",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "ViaVersion" || m.Version != "5.12.0" {
		t.Errorf("got %+v", *m)
	}
}

func TestReadMetaIgnoresNestedKeys(t *testing.T) {
	m, err := meta(t, jar(t, map[string]string{"plugin.yml": "commands:\n  name: nope\n  version: nope\nname: Real\nversion: 3\nmain: a.Real\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "Real" || m.Version != "3" {
		t.Errorf("got %+v, want the top-level name and version", *m)
	}
}

func TestReadMetaRejects(t *testing.T) {
	big := strings.Repeat("# padding\n", maxDescriptor/10+1)
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"not a zip", []byte("MZ this is an exe"), ErrNotJar},
		{"empty", nil, ErrNotJar},
		{"fabric mod", jar(t, map[string]string{"fabric.mod.json": "{}", "a/B.class": "x"}), ErrMod},
		{"forge mod", jar(t, map[string]string{"META-INF/mods.toml": ""}), ErrMod},
		{"neoforge mod", jar(t, map[string]string{"META-INF/neoforge.mods.toml": ""}), ErrMod},
		{"quilt mod", jar(t, map[string]string{"quilt.mod.json": "{}"}), ErrMod},
		{"library jar", jar(t, map[string]string{"a/B.class": "x"}), ErrNoPlugin},
		{"plugin.yml in a folder", jar(t, map[string]string{"x/plugin.yml": squaremapYML}), ErrNoPlugin},
		{"no main", jar(t, map[string]string{"plugin.yml": "name: A\nversion: 1\n"}), ErrBadPlugin},
		{"no version", jar(t, map[string]string{"plugin.yml": "name: A\nmain: a.A\n"}), ErrBadPlugin},
		{"block scalar name", jar(t, map[string]string{"plugin.yml": "name: |\n  A\nversion: 1\nmain: a.A\n"}), ErrBadPlugin},
	}
	for _, tt := range tests {
		if _, err := meta(t, tt.data); !errors.Is(err, tt.want) {
			t.Errorf("%s: err %v, want %v", tt.name, err, tt.want)
		}
	}

	if _, err := meta(t, jar(t, map[string]string{"plugin.yml": big + squaremapYML})); err == nil || !strings.Contains(err.Error(), "64 KB") {
		t.Errorf("huge plugin.yml: err %v, want a size error", err)
	}
	if _, err := ReadMeta(bytes.NewReader(nil), MaxJarSize+1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized jar: err %v, want ErrTooLarge", err)
	}
}

// A small zip can hold a huge number of empty entries, which costs memory
// to parse even though the file is under the size cap.
func TestReadMetaEntryLimit(t *testing.T) {
	many := func(n int) []byte {
		files := map[string]string{"plugin.yml": squaremapYML}
		for i := len(files); i < n; i++ {
			files[fmt.Sprintf("e/%d", i)] = ""
		}
		return jar(t, files)
	}
	if _, err := meta(t, many(MaxJarEntries+1)); !errors.Is(err, ErrTooManyEntries) {
		t.Errorf("%d entries: err %v, want ErrTooManyEntries", MaxJarEntries+1, err)
	}
	if _, err := meta(t, many(MaxJarEntries)); err != nil {
		t.Errorf("%d entries: %v, want it read", MaxJarEntries, err)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	if m, err := LoadManifest(filepath.Join(dir, "none.json")); m != nil || err != nil {
		t.Errorf("missing manifest: %v, %v, want nil, nil", m, err)
	}

	good := `{"plugins":[{"name":"A","version":"1","file":"A-1.jar","sha256":"` + strings.Repeat("ab", 32) + `","source":"https://example.com/A-1.jar"}]}`
	m, err := LoadManifest(writeManifest(t, dir, good))
	if err != nil || len(m.Plugins) != 1 || m.Plugins[0].File != "A-1.jar" {
		t.Fatalf("good manifest: %+v, %v", m, err)
	}

	entry := func(file, sha, source string) string {
		return `{"name":"A","version":"1","file":"` + file + `","sha256":"` + sha + `","source":"` + source + `"}`
	}
	ok := strings.Repeat("ab", 32)
	for name, body := range map[string]string{
		"folder in file": `{"plugins":[` + entry("../A.jar", ok, "https://x/A.jar") + `]}`,
		"not a jar":      `{"plugins":[` + entry("A.sh", ok, "https://x/A.sh") + `]}`,
		"short sha":      `{"plugins":[` + entry("A.jar", "abc", "https://x/A.jar") + `]}`,
		"upper sha":      `{"plugins":[` + entry("A.jar", strings.ToUpper(ok), "https://x/A.jar") + `]}`,
		"http source":    `{"plugins":[` + entry("A.jar", ok, "http://x/A.jar") + `]}`,
		"twice":          `{"plugins":[` + entry("A.jar", ok, "https://x/A.jar") + `,` + entry("A.jar", ok, "https://x/A.jar") + `]}`,
		"unknown field":  `{"plugins":[],"extra":1}`,
		"not json":       `plugins:`,
	} {
		if _, err := LoadManifest(writeManifest(t, dir, body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	a := jar(t, map[string]string{"plugin.yml": "name: A\nversion: 1\nmain: a.A\n"})
	b := jar(t, map[string]string{"plugin.yml": "name: B\nversion: 2\nmain: b.B\n"})
	c := jar(t, map[string]string{"plugin.yml": "name: C\nversion: 3\nmain: c.C\n"})
	mod := jar(t, map[string]string{"fabric.mod.json": "{}"})
	for name, data := range map[string][]byte{"A-1.jar": a, "B-2.jar": b, "C-3.jar": c, "mod.jar": mod, "notes.txt": []byte("hi")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Paper ignores subfolders, and so does List.
	if err := os.MkdirAll(filepath.Join(dir, "update"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update", "A-1.jar"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{Plugins: []Entry{
		{Name: "A", Version: "1", File: "A-1.jar", SHA256: sum(a), Source: "https://x/A-1.jar"},
		{Name: "B", Version: "2", File: "B-2.jar", SHA256: sum(a), Source: "https://x/B-2.jar"}, // wrong on purpose
		{Name: "D", Version: "4", File: "D-4.jar", SHA256: sum(c), Source: "https://x/D-4.jar"},
	}}
	got, err := List(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Status{
		"A-1.jar": StatusPinned,
		"B-2.jar": StatusChanged,
		"C-3.jar": StatusUnpinned,
		"D-4.jar": StatusMissing,
		"mod.jar": StatusInvalid,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d plugins, want %d: %+v", len(got), len(want), got)
	}
	for i, p := range got {
		if i > 0 && got[i-1].File >= p.File {
			t.Errorf("not sorted: %s before %s", got[i-1].File, p.File)
		}
		if p.Status != want[p.File] {
			t.Errorf("%s: status %s, want %s", p.File, p.Status, want[p.File])
		}
	}
	if got[0].SHA256 != sum(a) || got[0].Meta.Name != "A" || got[0].Source != "https://x/A-1.jar" || got[0].Size != int64(len(a)) {
		t.Errorf("A-1.jar: %+v", got[0])
	}
	if p := got[4]; p.Problem != ErrMod.Error() || p.Meta != nil {
		t.Errorf("mod.jar: %+v", p)
	}

	// Without a manifest every plugin is unpinned.
	got, err = List(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Status != StatusUnpinned && p.Status != StatusInvalid {
			t.Errorf("no manifest: %s is %s", p.File, p.Status)
		}
	}

	if _, err := List(filepath.Join(dir, "nope"), nil); err == nil {
		t.Error("missing folder: no error")
	}
}

// TestRepoManifest checks the manifest the repo ships against the jars
// checked in next to it.
func TestRepoManifest(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "plugins")
	m, err := LoadManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("plugins/manifest.json is missing")
	}
	got, err := List(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.Status != StatusPinned {
			t.Errorf("%s: %s %s", p.File, p.Status, p.Problem)
			continue
		}
		for _, e := range m.Plugins {
			if e.File == p.File && (e.Name != p.Meta.Name || e.Version != p.Meta.Version) {
				t.Errorf("%s: manifest says %s %s, plugin.yml says %s %s", p.File, e.Name, e.Version, p.Meta.Name, p.Meta.Version)
			}
		}
	}
}
