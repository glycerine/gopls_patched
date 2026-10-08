// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/internal/gocommand"
)

// pathBoundary limits source discovery to the startup directory and Go's
// source roots. It is not an OS sandbox for subprocesses or cache writes.
type pathBoundary struct {
	mu    sync.RWMutex
	roots []string
}

func newPathBoundary(dir string) *pathBoundary {
	b := new(pathBoundary)
	b.add(dir)
	b.addGoEnv(build.Default.GOROOT, build.Default.GOPATH)
	return b
}

func (b *pathBoundary) addGoEnv(goroot, gopath string) {
	b.add(goroot)
	for _, dir := range filepath.SplitList(gopath) {
		b.add(dir)
	}
}

func (b *pathBoundary) add(dir string) {
	if dir == "" || !filepath.IsAbs(dir) {
		return
	}
	real, err := resolvePath(dir)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, root := range b.roots {
		if root == real {
			return
		}
	}
	b.roots = append(b.roots, real)
}

// resolvePath also handles new files and directories: resolve the nearest
// existing ancestor, then append the missing suffix. Other errors fail closed.
func resolvePath(path string) (string, error) {
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// EvalSymlinks fails for a dangling link. Preserve its target rather than
	// treating the link itself as a new file inside its parent directory.
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return resolvePath(target)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = resolvePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}

func (b *pathBoundary) pathAllowed(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	real, err := resolvePath(path)
	if err != nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, root := range b.roots {
		rel, err := filepath.Rel(root, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

var errOutsideSourceRoots = errors.New("outside the gopls startup directory, GOROOT, and GOPATH")

// CheckPath rejects paths outside the session's source roots.
func (s *Session) CheckPath(uri protocol.DocumentURI) error {
	if !s.pathAllowed(uri.Path()) {
		return fmt.Errorf("%s: %w", uri.Path(), errOutsideSourceRoots)
	}
	return nil
}

func (s *Session) pathAllowed(path string) bool  { return s.boundary.pathAllowed(path) }
func (s *Snapshot) pathAllowed(path string) bool { return s.view.boundary.pathAllowed(path) }

// pathAllowed lets discovery helpers honor a restricted file source. Synthetic
// file sources used to test the workspace algorithm need not implement it.
func pathAllowed(fs file.Source, path string) bool {
	if bounded, ok := fs.(interface{ pathAllowed(string) bool }); ok {
		return bounded.pathAllowed(path)
	}
	return true
}

type boundedSource struct {
	file.Source
	*pathBoundary
}

func (fs boundedSource) ReadFile(ctx context.Context, uri protocol.DocumentURI) (file.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !fs.pathAllowed(uri.Path()) {
		return brokenFile{uri: uri, err: fmt.Errorf("source path %s is outside the gopls source roots", uri.Path())}, nil
	}
	return fs.Source.ReadFile(ctx, uri)
}

func (s *Session) ReadFile(ctx context.Context, uri protocol.DocumentURI) (file.Handle, error) {
	return (boundedSource{s.overlayFS, s.boundary}).ReadFile(ctx, uri)
}

func (s *Session) checkFolder(folder *Folder) error {
	s.boundary.addGoEnv(folder.Env.GOROOT, folder.Env.GOPATH)
	if err := s.CheckPath(folder.Dir); err != nil {
		return err
	}
	if folder.Env.GOMODCACHE != "" {
		if err := s.CheckPath(protocol.URIFromPath(folder.Env.GOMODCACHE)); err != nil {
			return err
		}
	}
	return nil
}

// FetchGoEnv prevents bootstrap Go commands from inheriting an excluded
// parent workspace before the view definition has been computed.
func (s *Session) FetchGoEnv(ctx context.Context, folder protocol.DocumentURI, opts *settings.Options) (*GoEnv, error) {
	s.boundary.addGoEnv(opts.Env["GOROOT"], opts.Env["GOPATH"])
	// The executable's GOROOT can differ from the toolchain that built gopls.
	// Query it in the allowed startup directory before checking the folder.
	var goroot, gopath string
	probeEnv := append(opts.EnvSlice(), "GOWORK=off", "GO111MODULE=off")
	if err := loadGoEnv(ctx, s.cache.startupDir, probeEnv, new(gocommand.Runner), map[string]*string{"GOROOT": &goroot, "GOPATH": &gopath}); err != nil {
		return nil, err
	}
	s.boundary.addGoEnv(goroot, gopath)
	if err := s.CheckPath(folder); err != nil {
		return nil, err
	}
	explicit, ok := opts.Env["GOWORK"]
	if !ok {
		explicit = os.Getenv("GOWORK")
	}
	work := explicit
	if work != "" && work != "off" {
		if err := s.CheckPath(protocol.URIFromPath(work)); err != nil {
			return nil, err
		}
	} else if work == "" {
		uri, err := findRootPattern(ctx, folder, "go.work", s)
		if err != nil {
			return nil, err
		}
		work = "off"
		if uri != "" {
			work = uri.Path()
		}
	}
	probe := opts.Clone()
	if probe.Env == nil {
		probe.Env = make(map[string]string)
	}
	probe.Env["GOWORK"] = work
	env, err := FetchGoEnv(ctx, folder, probe)
	if err != nil {
		return nil, err
	}
	env.ExplicitGOWORK = explicit
	return env, nil
}

// watchPattern gives every watcher an explicit allowed base directory. A bare
// glob would otherwise be applied to all client folders, including rejected
// folders outside the startup boundary.
func (b *pathBoundary) watchPattern(pattern protocol.RelativePattern, folder protocol.DocumentURI) (protocol.RelativePattern, bool) {
	path := filepath.FromSlash(pattern.Pattern)
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return pattern, false
		}
	}
	if filepath.IsAbs(path) {
		// Split the fixed directory prefix from the glob suffix.
		prefix := path
		if i := strings.IndexAny(path, "*?[{"); i >= 0 {
			prefix = path[:i]
		}
		dir := filepath.Dir(prefix)
		if strings.HasSuffix(prefix, string(filepath.Separator)) {
			dir = filepath.Clean(prefix)
		}
		if !b.pathAllowed(dir) {
			return pattern, false
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return pattern, false
		}
		pattern.BaseURI = protocol.URIFromPath(dir)
		pattern.Pattern = filepath.ToSlash(rel)
	} else if pattern.BaseURI == "" {
		pattern.BaseURI = folder
	}
	return pattern, b.pathAllowed(pattern.BaseURI.Path())
}
