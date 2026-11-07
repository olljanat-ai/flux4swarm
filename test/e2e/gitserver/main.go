// Command gitserver is a minimal Git smart-HTTP server (protocol v0/v1, no
// auth) for the e2e tests. Flux's GitRepository only accepts http(s):// and
// ssh:// URLs, and the small alpine/git image ships without git-http-backend,
// so this implements the smart-HTTP endpoints on top of
// `git upload-pack|receive-pack --stateless-rpc`.
//
// It serves every bare repo below -root (it never creates repos):
//
//	GET  /<repo>/info/refs?service=git-upload-pack|git-receive-pack
//	POST /<repo>/git-upload-pack
//	POST /<repo>/git-receive-pack
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	root := flag.String("root", "/srv/git", "directory containing bare repos")
	flag.Parse()
	// The bind-mounted repos are usually owned by another uid.
	os.Setenv("GIT_CONFIG_COUNT", "1")
	os.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	os.Setenv("GIT_CONFIG_VALUE_0", "*")
	// Never speak protocol v2 (go-git, used by Flux, does not).
	os.Unsetenv("GIT_PROTOCOL")

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { serve(w, r, *root) })
	log.Printf("serving %s on %s", *root, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func serve(w http.ResponseWriter, r *http.Request, root string) {
	var repoPath, service string
	var advertise bool
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
		service = strings.TrimPrefix(r.URL.Query().Get("service"), "git-")
		repoPath, advertise = strings.TrimSuffix(r.URL.Path, "/info/refs"), true
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		repoPath, service = strings.TrimSuffix(r.URL.Path, "/git-upload-pack"), "upload-pack"
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		repoPath, service = strings.TrimSuffix(r.URL.Path, "/git-receive-pack"), "receive-pack"
	default:
		http.NotFound(w, r)
		return
	}
	if service != "upload-pack" && service != "receive-pack" {
		http.Error(w, "only smart HTTP (git-upload-pack / git-receive-pack) is supported", http.StatusForbidden)
		return
	}
	dir := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+repoPath)))
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}

	args := []string{service, "--stateless-rpc"}
	if advertise {
		args = append(args, "--advertise-refs")
	}
	cmd := exec.CommandContext(r.Context(), "git", append(args, dir)...)
	cmd.Stderr = os.Stderr
	if !advertise {
		// Read the whole request first: git answers only after its input, and
		// net/http may close the body once the response has started.
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}
		in, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cmd.Stdin = bytes.NewReader(in)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	if advertise {
		w.Header().Set("Content-Type", "application/x-git-"+service+"-advertisement")
		hdr := "# service=git-" + service + "\n"
		fmt.Fprintf(w, "%04x%s0000", len(hdr)+4, hdr)
	} else {
		w.Header().Set("Content-Type", "application/x-git-"+service+"-result")
	}
	if _, err := io.Copy(w, out); err != nil {
		log.Printf("%s %s: copy: %v", r.Method, r.URL.Path, err)
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("%s %s: git: %v", r.Method, r.URL.Path, err)
	}
	log.Printf("%s %s", r.Method, r.URL.String())
}
