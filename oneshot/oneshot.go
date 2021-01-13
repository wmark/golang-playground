// Package main really is a sandbox implementation for one run only,
// to be instantiated to then be contained and discarded by systemd.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/playground/sandbox/sandboxtypes"

	"github.com/coreos/go-systemd/v22/activation"
	netutil "github.com/wmark/go.netutil"
)

// The default webserver address that will be used in case
// this server has not been passed a socket by the invoker.
const defaultAddr = "localhost:0"

const (
	maxBinarySize    = 100 << 20
	startTimeout     = 30 * time.Second
	runTimeout       = 5 * time.Second
	maxOutputSize    = 100 << 20
	memoryLimitBytes = 100 << 20
)

const (
	errTooMuchOutput constError = "Output too large"
	errRunTimeout    constError = "Timeout running program"
)

// constError really is just an error message.
type constError string

// Error implements the error interface.
func (e constError) Error() string { return string(e) }

func main() {
	ctx, cancelCtxFn := context.WithCancel(context.Background())
	defer cancelCtxFn()
	launchHTTPEndpoint(ctx, defaultAddr)
}

func launchHTTPEndpoint(ctx context.Context, fallbackAddr string) {
	mux := http.NewServeMux()
	// Not supported are /statusz, /health, and /healthz
	// because this process gets torn down after one run.
	mux.Handle("/", http.HandlerFunc(rootHandler))
	mux.Handle("/run", http.HandlerFunc(runHandler))

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	server.SetKeepAlivesEnabled(false)

	ephemeralServerCtx, cancelServerCtx := context.WithCancel(ctx)
	defer cancelServerCtx()

	var ln net.Listener
	if _, socketActivated := os.LookupEnv("LISTEN_FDS"); socketActivated {
		log.SetFlags(0) // Rely on the init daemon to annotate loglines.

		fds := activation.Files(true) // NOT sockets.
		if len(fds) < 1 {
			log.Fatalf("Socket activated, but not been given any FD")
		}

		listener, err := netutil.AcceptedConnection(fds[0])
		if err != nil {
			log.Fatalf("Unable to convert the given FD to a Listener. Err: %v", err)
		}
		ln = listener
	} else {
		listener, err := net.Listen("tcp", defaultAddr)
		if err != nil {
			log.Fatalf("net.Listen %s: %v", defaultAddr, err)
		}
		log.Printf("Listening on: %s", listener.Addr().String())
		ln = listener
	}

	lingerCtx := netutil.NewIdleTracker(ephemeralServerCtx, 0)
	go func() {
		<-lingerCtx.Done()
		tearDownCtx, _ := context.WithTimeout(ctx, 10*time.Second)
		server.Shutdown(tearDownCtx)
	}()
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			cancelServerCtx()
		}
		lingerCtx.ConnState(conn, state)
	}

	switch err := server.Serve(ln); err {
	case nil, http.ErrServerClosed, os.ErrClosed:
		return
	default:
		log.Fatalf("server.Serve: %v", err)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	io.WriteString(w, "Hi from the one-shot sandbox\n")
}

func runHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")
	// Adapted from sandbox/sandbox.go
	if r.Method != "POST" {
		http.Error(w, "expected a POST", http.StatusBadRequest)
		return
	}

	bin, err := ioutil.ReadAll(http.MaxBytesReader(w, r.Body, maxBinarySize))
	if err != nil {
		log.Printf("failed to read request body: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tmpfile, err := ioutil.TempFile("", "play-*")
	if err != nil {
		log.Printf("Cannot open a new file for the binary, is TMPDIR writable? Err: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpfile.Truncate(int64(len(bin)))
	tmpfile.Chmod(os.FileMode(0750))
	if _, err := tmpfile.Write(bin); err != nil {
		log.Printf("tmpfile.Write: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		os.Remove(tmpfile.Name())
		return
	}
	if err := tmpfile.Close(); err != nil {
		log.Printf("tmpfile.Close: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		os.Remove(tmpfile.Name())
		return
	}

	cmd := exec.Command(tmpfile.Name(), r.Header["X-Argument"]...)
	if runtime.GOOS == "linux" {
		// Standardize the UID and GID the binary will experience.
		// This is in no shape or form a proper isolation from the host.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWUTS |
				syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
			Credential:  &syscall.Credential{Uid: 0, Gid: 0},
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
	}
	stderr, _ := cmd.StderrPipe()
	stdout, _ := cmd.StdoutPipe()

	ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
	// Binds the lifetime of this request handler to that of the binary it called.
	closed := make(chan struct{})
	defer func() {
		cancel()
		<-closed
	}()
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		os.Remove(cmd.Path)
		close(closed)
	}()

	if err := cmd.Start(); err != nil {
		sendError(w, err.Error())
		return
	}
	res := &sandboxtypes.Response{}
	res.Stderr, _ = ioutil.ReadAll(stderr)
	res.Stdout, _ = ioutil.ReadAll(stdout)
	err = cmd.Wait()
	select {
	case <-ctx.Done():
		// Timed out or canceled before or exactly as Wait returned.
		// Either way, treat it as a timeout.
		sendError(w, "timeout running program")
		return
	default:
		cancel()
	}

	if err != nil {
		if len(res.Stderr)+len(res.Stdout) > maxOutputSize {
			// Do not send truncated output, just send the error.
			sendError(w, errTooMuchOutput.Error())
			return
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			http.Error(w, "unknown error running the given binary", http.StatusInternalServerError)
			return
		}
		res.ExitCode = ee.ExitCode()
	}
	sendResponse(w, res)
}

// The following is copied from sandbox/sandbix.go

func sendError(w http.ResponseWriter, errMsg string) {
	sendResponse(w, &sandboxtypes.Response{Error: errMsg})
}

func sendResponse(w http.ResponseWriter, r *sandboxtypes.Response) {
	jres, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		http.Error(w, "error encoding JSON", http.StatusInternalServerError)
		log.Printf("json marshal: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(jres)))
	w.Write(jres)
}
