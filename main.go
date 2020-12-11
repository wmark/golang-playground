// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/datastore"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/wmark/idletracker"
)

var log = newStdLogger()

var (
	runtests   = flag.Bool("runtests", false, "Run integration tests instead of Playground server.")
	backendURL = flag.String("backend-url", "", "URL for sandbox backend that runs Go binaries.")
)

func main() {
	flag.Parse()
	s, err := newServer(func(s *server) error {
		pid := projectID()
		if pid == "" {
			s.db = &inMemStore{}
		} else {
			c, err := datastore.NewClient(context.Background(), pid)
			if err != nil {
				return fmt.Errorf("could not create cloud datastore client: %v", err)
			}
			s.db = cloudDatastore{client: c}
		}
		if caddr := os.Getenv("MEMCACHED_ADDR"); caddr != "" {
			s.cache = newGobCache(caddr)
			log.Printf("App (project ID: %q) is caching results", pid)
		} else {
			s.cache = (*gobCache)(nil) // Use a no-op cache implementation.
			log.Printf("App (project ID: %q) is NOT caching results", pid)
		}
		s.log = log
		return nil
	})
	if err != nil {
		log.Fatalf("Error creating server: %v", err)
	}

	if *runtests {
		s.test()
		return
	}
	if *backendURL != "" {
		// TODO(golang.org/issue/25224) - Remove environment variable and use a flag.
		os.Setenv("SANDBOX_BACKEND_URL", *backendURL)
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()
	server := &http.Server{
		Handler: s,
	}

	var ln net.Listener
	if _, socketActivated := os.LookupEnv("LISTEN_FDS"); socketActivated {
		listeners, err := activation.Listeners()
		if len(listeners) < 1 || err != nil {
			log.Fatalf("Socket activated, but without any listener. Err: %v", err)
		}
		ln = listeners[0]

		lingerCtx := idletracker.NewIdleTracker(ctx, 15*time.Minute)
		go func() {
			<-lingerCtx.Done()
			tearDownCtx, _ := context.WithTimeout(ctx, 10*time.Second)
			server.Shutdown(tearDownCtx)
		}()
		server.ConnState = lingerCtx.ConnState
	} else {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}

		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatalf("net.Listen %s: %v", ":"+port, err)
		}
		log.Printf("Listening on: %v ...", port)
		ln = listener
	}

	// Get the backend dialer warmed up. This starts
	// RegionInstanceGroupDialer queries and health checks.
	go sandboxBackendClient()

	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("ListenAndServe: %v", err)
	}
}

func projectID() string {
	id, err := metadata.ProjectID()
	if err != nil && os.Getenv("GAE_INSTANCE") != "" {
		log.Fatalf("Could not determine the project ID: %v", err)
	}
	return id
}
