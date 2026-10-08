// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/testenv"
)

func TestPathBoundary(t *testing.T) {
	root := t.TempDir()
	startup := filepath.Join(root, "project")
	goroot := filepath.Join(root, "goroot")
	gopath1 := filepath.Join(root, "gopath1")
	gopath2 := filepath.Join(root, "gopath2")
	outside := filepath.Join(root, "project-other")
	for _, dir := range []string{startup, goroot, gopath1, gopath2, outside} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	b := new(pathBoundary)
	b.add(startup)
	b.addGoEnv(goroot, gopath1+string(os.PathListSeparator)+gopath2)
	for _, test := range []struct {
		path string
		want bool
	}{
		{startup, true}, {filepath.Join(startup, "new", "file.go"), true},
		{goroot, true}, {filepath.Join(gopath1, "pkg", "mod"), true}, {gopath2, true},
		{root, false}, {outside, false}, {filepath.Join(startup, "..", "secret.go"), false}, {"relative.go", false},
	} {
		if got := b.pathAllowed(test.path); got != test.want {
			t.Errorf("pathAllowed(%q) = %v, want %v", test.path, got, test.want)
		}
	}
	t.Run("symlinks", func(t *testing.T) {
		link := filepath.Join(startup, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Skip(err)
		}
		for _, path := range []string{link, filepath.Join(link, "new.go"), filepath.Join(link, "missing", "new.go")} {
			if b.pathAllowed(path) {
				t.Errorf("allowed escaping symlink %q", path)
			}
		}
		dangling := filepath.Join(startup, "dangling")
		if err := os.Symlink(filepath.Join(outside, "missing"), dangling); err != nil {
			t.Fatal(err)
		}
		if b.pathAllowed(dangling) || b.pathAllowed(filepath.Join(dangling, "new.go")) {
			t.Error("allowed dangling symlink outside source roots")
		}
		inside := filepath.Join(startup, "inside")
		if err := os.Symlink(gopath1, inside); err != nil {
			t.Fatal(err)
		}
		if !b.pathAllowed(filepath.Join(inside, "new.go")) {
			t.Error("rejected symlink into GOPATH")
		}
	})
}

func TestBoundedWorkspaceDiscovery(t *testing.T) {
	for _, test := range []struct {
		name, parentWork, mod, work string
		explicit, wantError         bool
	}{
		{name: "parent workspace", parentWork: "go 1.18\nuse ./other\n", mod: "module example.com/project\ngo 1.18\n"},
		{name: "parent module"},
		{name: "outside workspace use", work: "go 1.18\nuse ../other\n", wantError: true},
		{name: "outside module replace", mod: "module example.com/project\ngo 1.18\nreplace example.com/other => ../other\n", wantError: true},
		{name: "outside workspace replace", mod: "module example.com/project\ngo 1.18\n", work: "go 1.18\nuse .\nreplace example.com/other => ../other\n", wantError: true},
		{name: "explicit outside workspace", explicit: true, wantError: true},
		{name: "inside workspace", mod: "module example.com/project\ngo 1.18\n", work: "go 1.18\nuse .\n"},
		{name: "malformed module", mod: "invalid module file\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "project")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "go.mod"), "module example.com/parent\ngo 1.18\n")
			if test.parentWork != "" {
				write(filepath.Join(root, "go.work"), test.parentWork)
			}
			if test.mod != "" {
				write(filepath.Join(dir, "go.mod"), test.mod)
			}
			if test.work != "" {
				write(filepath.Join(dir, "go.work"), test.work)
			}
			b := new(pathBoundary)
			b.add(dir)
			fs := boundedSource{newMemoizedFS(), b}
			folder := &Folder{Dir: protocol.URIFromPath(dir), Options: settings.DefaultOptions()}
			if test.explicit {
				folder.Env.ExplicitGOWORK = filepath.Join(root, "go.work")
			}
			def, err := defineView(t.Context(), fs, folder, nil)
			if test.wantError {
				if err == nil {
					t.Fatal("accepted outside module/workspace")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if def.root.Path() != dir {
				t.Errorf("root = %s, want %s", def.root.Path(), dir)
			}
			view := &View{viewDefinition: def, boundary: b}
			if test.work == "" && !slices.Contains(view.Env(), "GOWORK=off") {
				t.Error("Go command may rediscover parent workspace")
			}
			if test.mod == "" && !slices.Contains(view.Env(), "GO111MODULE=off") {
				t.Error("Go command may rediscover parent module")
			}
			if _, err := goModModules(t.Context(), protocol.URIFromPath(filepath.Join(root, "go.mod")), fs); err == nil {
				t.Error("read outside module")
			}
		})
	}
}

func TestSessionSourceBoundary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	outside := protocol.URIFromPath(filepath.Join(root, "outside.go"))
	if err := os.WriteFile(outside.Path(), []byte("package outside"), 0644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(t.Context(), NewWithWorkingDirectory(nil, dir))
	t.Cleanup(func() { s.Shutdown(t.Context()) })
	if !errors.Is(s.CheckPath(outside), errOutsideSourceRoots) {
		t.Error("accepted outside path")
	}
	fh, err := s.ReadFile(t.Context(), outside)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Content(); err == nil {
		t.Fatal("read outside source")
	}
	folder := &Folder{Dir: protocol.URIFromPath(root), Options: settings.DefaultOptions()}
	if _, _, release, err := s.NewView(t.Context(), folder); err == nil {
		release()
		t.Fatal("accepted outside workspace folder")
	}
}

func TestModuleCacheScanBoundary(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "project", "modcache")
	inside := filepath.Join(cacheDir, "example.com", "inside@v0.1.0")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{inside, outside} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte("package p"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(cacheDir, "example.com", "escape@v0.1.0")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skip(err)
	}
	b := new(pathBoundary)
	b.add(filepath.Join(root, "project"))
	cache := imports.NewDirInfoCache()
	imports.ScanModuleCache(cacheDir, cache, nil, b.pathAllowed)
	found := false
	for _, key := range cache.Keys() {
		if key == escape {
			t.Error("scanned symlink outside source roots")
		}
		if key == inside {
			found = true
		}
	}
	if !found {
		t.Error("did not scan allowed module")
	}
}

func TestBoundedGoEnv(t *testing.T) {
	testenv.NeedsExec(t)
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// An invalid ancestor workspace must not affect bootstrap Go commands.
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("invalid workspace\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/project\ngo 1.18\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(t.Context(), NewWithWorkingDirectory(nil, dir))
	t.Cleanup(func() { s.Shutdown(t.Context()) })
	if _, err := s.FetchGoEnv(t.Context(), protocol.URIFromPath(dir), settings.DefaultOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSourceCacheBoundary(t *testing.T) {
	root := t.TempDir()
	gopath := filepath.Join(root, "gopath")
	b := new(pathBoundary)
	b.add(gopath)
	def := &viewDefinition{typ: GoModView, folder: &Folder{Env: GoEnv{GOPATH: gopath, GOCACHE: filepath.Join(root, "outside")}}}
	v := &View{viewDefinition: def, boundary: b}
	want := "GOCACHE=" + filepath.Join(gopath, "pkg", "gopls-build-cache")
	if !slices.Contains(v.sourceEnv(), want) {
		t.Errorf("sourceEnv = %v, want %s", v.sourceEnv(), want)
	}
	def.folder.Env.GOCACHE = filepath.Join(gopath, "cache")
	for _, env := range v.sourceEnv() {
		if len(env) >= len("GOCACHE=") && env[:len("GOCACHE=")] == "GOCACHE=" {
			t.Errorf("overrode allowed cache: %s", env)
		}
	}
}

func TestWatcherBoundary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	b := new(pathBoundary)
	b.add(dir)
	folder := protocol.URIFromPath(dir)
	for _, test := range []struct {
		pattern protocol.RelativePattern
		want    bool
	}{
		{protocol.RelativePattern{Pattern: "**/*.go"}, true},
		{protocol.RelativePattern{Pattern: "../**/*.go"}, false},
		{protocol.RelativePattern{Pattern: filepath.ToSlash(filepath.Join(root, "**", "*.go"))}, false},
		{protocol.RelativePattern{Pattern: filepath.ToSlash(filepath.Join(dir, "**", "*.go"))}, true},
		{protocol.RelativePattern{BaseURI: protocol.URIFromPath(root), Pattern: "**/*.go"}, false},
	} {
		got, ok := b.watchPattern(test.pattern, folder)
		if ok != test.want {
			t.Errorf("watchPattern(%v) accepted=%v, want %v", test.pattern, ok, test.want)
		}
		if ok && (got.BaseURI == "" || !b.pathAllowed(got.BaseURI.Path())) {
			t.Errorf("unbounded watcher: %v", got)
		}
	}
}

func TestRootOutput(t *testing.T) {
	dir := t.TempDir()
	c := NewWithWorkingDirectory(nil, dir)
	var output bytes.Buffer
	c.SetRootOutput(&output)
	s := NewSession(t.Context(), c)
	defer s.Shutdown(t.Context())
	gp1, gp2 := filepath.Join(dir, "gopath1"), filepath.Join(dir, "gopath2")
	s.boundary.addGoEnv("", gp1+string(os.PathListSeparator)+gp2)
	s.boundary.add(gp1) // repeated roots must not produce repeated output
	got := strings.Split(strings.TrimSpace(output.String()), "\n")
	for i, line := range got {
		root, ok := strings.CutPrefix(line, "showroots: ")
		if !ok {
			t.Fatalf("unprefixed root output %q", line)
		}
		got[i] = root
	}
	want := slices.Clone(s.boundary.roots)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("printed roots %v, want %v", got, want)
	}
	for _, root := range got {
		if !s.pathAllowed(root) {
			t.Errorf("printed forbidden root %q", root)
		}
	}
	before := output.String()
	s2 := NewSession(t.Context(), c)
	defer s2.Shutdown(t.Context())
	if output.String() != before {
		t.Error("repeated startup roots for a second session")
	}
}
