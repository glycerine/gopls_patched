// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/internal/testenv"
)

func TestShowRoots(t *testing.T) {
	testenv.NeedsExec(t)
	t.Parallel()
	for _, args := range [][]string{{"-showroots"}, {"serve", "-showroots"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := writeTree(t, "")
			gp1, gp2 := filepath.Join(dir, "gopath1"), filepath.Join(dir, "gopath2")
			goroot, err := filepath.EvalSymlinks(build.Default.GOROOT)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{dir: true, goroot: true, gp1: true, gp2: true}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			process := exec.CommandContext(ctx, os.Args[0], args...)
			process.Dir = dir
			process.Env = append(os.Environ(), "ENTRYPOINT=goplsMain", "GOPATH="+gp1+string(os.PathListSeparator)+gp2)
			var stderr bytes.Buffer
			process.Stderr = &stderr
			stdin, err := process.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := process.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = process.Wait(); close(done) }()
			t.Cleanup(func() { cancel(); <-done })

			// The initialize response proves that printing roots does not exit the
			// server. Keep stdin open until the response arrives.
			request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`
			if _, err := fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(request), request); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				reader := bufio.NewReader(stdout)
				roots := make(map[string]bool)
				for range len(want) {
					line, err := reader.ReadString('\n')
					if err != nil {
						result <- err
						return
					}
					if !strings.HasPrefix(line, "showroots: ") {
						result <- fmt.Errorf("unprefixed root line %q", line)
						return
					}
					root := strings.TrimSpace(strings.TrimPrefix(line, "showroots: "))
					if !want[root] || roots[root] {
						result <- fmt.Errorf("unexpected or duplicate root %q", root)
						return
					}
					roots[root] = true
				}
				header, err := reader.ReadString('\n')
				if err != nil {
					result <- err
					return
				}
				length, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "Content-Length:")))
				if err != nil {
					result <- fmt.Errorf("expected LSP response after roots, got %q", header)
					return
				}
				if _, err := reader.ReadString('\n'); err != nil {
					result <- err
					return
				}
				body := make([]byte, length)
				if _, err := io.ReadFull(reader, body); err != nil {
					result <- err
					return
				}
				var response struct {
					ID     int
					Result json.RawMessage
					Error  json.RawMessage
				}
				if err := json.Unmarshal(body, &response); err != nil {
					result <- err
					return
				}
				if response.ID != 1 || len(response.Result) == 0 || len(response.Error) != 0 {
					result <- fmt.Errorf("unexpected initialize response: %s", body)
					return
				}
				result <- nil
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("timed out waiting for roots and initialize response")
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatalf("gopls exited with %v: %s", waitErr, stderr.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("server did not stop after stdin closed")
			}
		})
	}
}

func TestRootsQuietByDefault(t *testing.T) {
	t.Parallel()
	res := gopls(t, writeTree(t, ""), "serve")
	res.checkExit(true)
	if res.stdout != "" {
		t.Errorf("unexpected stdout without -showroots: %q", res.stdout)
	}
}

func TestShowRootsStats(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-showroots", "stats"}, {"stats", "-showroots"}, {"stats", "-anon", "-showroots"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := writeTree(t, "-- go.mod --\nmodule example.com\ngo 1.18\n-- a.go --\npackage a\n")
			// Put all source/build cache roots in the writable test workspace.
			env := []string{"GOPATH=" + filepath.Join(dir, "gopath"), "GOMODCACHE=" + filepath.Join(dir, "gopath", "pkg", "mod"), "GOCACHE=" + os.Getenv("GOCACHE")}
			res := goplsWithEnv(t, dir, env, args...)
			res.checkExit(true)
			found := false
			output := res.stdout
			for strings.HasPrefix(output, "showroots: ") {
				line, rest, _ := strings.Cut(output, "\n")
				if line == "showroots: ." {
					found = true
				}
				output = rest
			}
			if !found {
				t.Fatalf("startup root missing from stdout: %s", res.stdout)
			}
			var stats map[string]any
			if err := json.Unmarshal([]byte(output), &stats); err != nil {
				t.Fatalf("stats did not complete after root output: %v\n%s", err, res.stdout)
			}
			if _, ok := stats["WorkspaceStats"]; !ok {
				t.Error("missing WorkspaceStats")
			}
		})
	}
}
