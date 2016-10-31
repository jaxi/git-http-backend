package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// Service defines the Git Smart HTTP request by the given method and pattern
type Service struct {
	Method  string
	Pattern *regexp.Regexp
	Handler func(s Service, w http.ResponseWriter, r *http.Request)
}

// GitSmartHTTPConfig is the configuration for GitSmartHTTP
type GitSmartHTTPConfig struct {
	RootPath    string
	ReceivePack bool
	UploadPack  bool
}

// GitSmartHTTP acts as an Git Smart HTTP server's handler and deal
// with all kinds of Git HTTP request
type GitSmartHTTP struct {
	Services []Service
	*GitSmartHTTPConfig
}

// NewGitSmartHTTP returns a GitSmartHTTP
func NewGitSmartHTTP(cfg *GitSmartHTTPConfig) GitSmartHTTP {
	gsh := GitSmartHTTP{
		GitSmartHTTPConfig: cfg,
	}

	gsh.Services = []Service{
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/HEAD$"),
			Handler: gsh.handleTextFile,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/info/packs$"),
			Handler: gsh.handleInfoPacks,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/info/refs$"),
			Handler: gsh.handleInfoRefs,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/objects/info/alternates$"),
			Handler: gsh.handleTextFile,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/objects/info/http-alternates$"),
			Handler: gsh.handleTextFile,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/objects/[0-9a-f]{2}/[0-9a-f]{38}$"),
			Handler: gsh.handleLooseObject,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/objects/pack/pack-[0-9a-f]{40}\\.pack$"),
			Handler: gsh.handlePackFile,
		},
		Service{
			Method:  "GET",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/objects/pack/pack-[0-9a-f]{40}\\.idx$"),
			Handler: gsh.handleIdxFile,
		},
		Service{
			Method:  "POST",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/git-upload-pack$"),
			Handler: gsh.handleServiceRPC,
		},
		Service{
			Method:  "POST",
			Pattern: regexp.MustCompile("(?P<repoPath>.*)/git-receive-pack$"),
			Handler: gsh.handleServiceRPC,
		},
	}
	return gsh
}

// ServerHttp implements the iServerHttp nterface of http.Handler
func (gsh GitSmartHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Log request
	log.Printf(`%s - - "%s %s %s"`, r.RemoteAddr, r.Method, r.URL.Path, r.Proto)

	for _, service := range gsh.Services {
		if service.Pattern.MatchString(r.URL.Path) {
			if r.Method == service.Method {
				service.Handler(service, w, r)
			} else {
				methodNotAllowed(w, r)
			}
			break
		}
	}
}

func (gsh GitSmartHTTP) handleTextFile(s Service, w http.ResponseWriter, r *http.Request) {
	gsh.sendFile(w, r, "text/plain", hdrNoCache())
}

func (gsh GitSmartHTTP) handleInfoPacks(s Service, w http.ResponseWriter, r *http.Request) {
	gsh.sendFile(w, r, "text/plain; charset=utf-8", hdrNoCache())
}

func (gsh GitSmartHTTP) handleLooseObject(s Service, w http.ResponseWriter, r *http.Request) {
	gsh.sendFile(w, r, "application/x-git-loose-object", hdrCacheForever())
}

func (gsh GitSmartHTTP) handlePackFile(s Service, w http.ResponseWriter, r *http.Request) {
	gsh.sendFile(w, r, "application/x-git-packed-objects", hdrCacheForever())
}

func (gsh GitSmartHTTP) handleIdxFile(s Service, w http.ResponseWriter, r *http.Request) {
	gsh.sendFile(w, r, "application/x-git-packed-objects-toc", hdrCacheForever())
}

func (gsh GitSmartHTTP) handleInfoRefs(s Service, w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	serviceType := r.FormValue("service")

	repoPath := path.Join(gsh.RootPath, s.Pattern.FindAllStringSubmatch(r.URL.Path, -1)[0][1])

	gs := NewGitRPCClient(&GitRPCClientConfig{
		Stream: false,
	})

	if gsh.serviceAccess(serviceType) {
		w.Header().Add("Content-Type", fmt.Sprintf("application/x-%s-advertisement", serviceType))
		setHeaders(w, hdrNoCache())
		w.WriteHeader(http.StatusOK)

		rpcCfg := map[string]struct{}{
			"advertise_refs": struct{}{},
		}

		if serviceType == uploadPack {
			gs.UploadPack(repoPath, rpcCfg)
		} else {
			gs.ReceivePack(repoPath, rpcCfg)
		}
		refs, _ := gs.Output()

		fmt.Fprint(w, pktWrite(fmt.Sprintf("# service=%s\n", serviceType)))
		fmt.Fprint(w, pktFlush())
		w.Write(refs)
	} else {
		gs.UploadPack(repoPath, map[string]struct{}{})
		gs.Output()

		gsh.sendFile(w, r, "text/plain; charset=utf-8", hdrNoCache())
	}
}

func (gsh GitSmartHTTP) handleServiceRPC(s Service, w http.ResponseWriter, r *http.Request) {
	fullPath := r.URL.Path

	repo := s.Pattern.FindAllStringSubmatch(fullPath, -1)[0][1]
	repoPath := path.Join(gsh.RootPath, repo)
	rpc := fullPath[len(repo)+1 : len(fullPath)]

	if !gsh.serviceAccess(rpc) {
		w.WriteHeader(http.StatusForbidden)
		w.Header().Set("Content-Type", "text/plain")
		return
	}

	reqBody, _ := ioutil.ReadAll(r.Body)

	gs := NewGitRPCClient(&GitRPCClientConfig{
		Stream: true,
	})
	if rpc == uploadPack {
		gs.UploadPack(repoPath, map[string]struct{}{})
	} else {
		gs.ReceivePack(repoPath, map[string]struct{}{})
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-git-%s-result", rpc))
	if err := gs.Start(); err != nil {
		fmt.Println("error!")
	}
	gs.StdinWriter.Write(reqBody)
	io.Copy(w, gs.StdoutReader)
	io.Copy(w, gs.StderrReader)

	gs.Wait()
}

func pktWrite(s string) string {
	sSize := strconv.FormatInt(int64(len(s)+4), 16)
	sSize = strings.Repeat("0", 4-len(sSize)%4) + sSize
	return sSize + s
}

func pktFlush() string {
	return "0000"
}

func (gsh GitSmartHTTP) sendFile(w http.ResponseWriter, r *http.Request, contentType string, hdr map[string]string) {
	fullPath := path.Join(gsh.RootPath, r.URL.Path)

	f, err := os.Open(fullPath)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain")
		http.NotFound(w, r)
		return
	}

	fInfo, err := f.Stat()
	if err != nil {
		fmt.Fprintf(w, "Cannot fetch file %v", err)
		return
	}

	size := strconv.FormatInt(fInfo.Size(), 10)
	mtime := fInfo.ModTime().Format(time.RFC850)

	setHeaders(w, hdr)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", size)
	w.Header().Set("Last-Modified", mtime)

	io.Copy(w, f)
}

func (gsh GitSmartHTTP) serviceAccess(service string) bool {
	if service == uploadPack {
		return gsh.UploadPack
	}

	if service == receivePack {
		return gsh.ReceivePack
	}

	return false
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	if r.Proto == "HTTP/1.1" {
		w.WriteHeader(http.StatusMethodNotAllowed)
	} else {
		w.WriteHeader(http.StatusBadRequest)
	}
}

func hdrNoCache() map[string]string {
	return map[string]string{
		"Expires":       "Fri, 01 Jan 1980 00:00:00 GMT",
		"Pragma":        "no-cache",
		"Cache-Control": "no-cache, max-age=0, must-revalidate",
	}
}

func hdrCacheForever() map[string]string {
	now := time.Now()
	expires := now.Add(31536000 * time.Second)

	return map[string]string{
		"Date":          now.Format(time.RFC850),
		"Expires":       expires.Format(time.RFC850),
		"Cache-Control": "public, max-age=31536000",
	}
}

func setHeaders(w http.ResponseWriter, hdr map[string]string) {
	for key, value := range hdr {
		w.Header().Set(key, value)
	}
}

func main() {
	pwd, _ := os.Getwd()
	gsh := NewGitSmartHTTP(&GitSmartHTTPConfig{
		UploadPack:  true,
		ReceivePack: true,
		RootPath:    path.Join(pwd, "testdir"),
	})

	mux := http.NewServeMux()
	mux.Handle("/", gsh)
	http.ListenAndServe(":8080", mux)
}