// Copyright 2017 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// pullhook runs git pull on a webhook.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	fsnotify "gopkg.in/fsnotify.v1"

	"github.com/bugsnag/osext"
	"github.com/google/go-github/github"
)

var start time.Time

// normalizeUTF8 returns valid UTF8 from potentially incorrectly encoded data
// from an untrusted process.
func normalizeUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	var out []byte
	for len(b) != 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError {
			out = append(out, b[:size]...)
		}
		b = b[size:]
	}
	return out
}

// roundTime returns time rounded at a value that makes sense to display to the
// user.
func roundTime(t time.Duration) time.Duration {
	if t < time.Millisecond {
		// Precise at 1ns.
		return t
	}
	if t < time.Second {
		// Precise at 1µs.
		return (t + time.Microsecond/2) / time.Microsecond * time.Microsecond
	}
	// Round at 1ms
	return (t + time.Millisecond/2) / time.Millisecond * time.Millisecond
}

// pullRepo tries to pull a repository if possible. If the pull failed, it
// deletes the checkout.
func pullRepo() (string, bool) {
	cmd := []string{"git", "pull", "--prune", "--quiet"}
	cmds := strings.Join(cmd, " ")
	log.Printf("- %s", cmds)
	c := exec.Command(cmd[0], cmd[1:]...)
	start := time.Now()
	out, err := c.CombinedOutput()
	duration := time.Since(start)
	exit := 0
	if err != nil {
		exit = -1
		if len(out) == 0 {
			out = []byte("<failure>\n" + err.Error() + "\n")
		}
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				exit = status.ExitStatus()
			}
		}
	}
	return fmt.Sprintf("$ %s  (exit:%d in %s)\n%s", cmds, exit, roundTime(duration), normalizeUTF8(out)), err == nil
}

// server is both the HTTP server and the task queue server.
type server struct {
	WebHookSecret string
	mu            sync.Mutex     // Set when a check is running
	wg            sync.WaitGroup // Set for each pending task.
}

// ServeHTTP handles all HTTP requests and triggers a task if relevant.
//
// While the task is started asynchronously, a synchronous status update is
// done so the user is immediately alerted that the task is pending on the
// host. Only one task runs at a time.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%-4s %-21s %s", r.Method, r.RemoteAddr, r.URL.Path)
	defer r.Body.Close()
	// The path must be the root path.
	if r.URL.Path != "" && r.URL.Path != "/" {
		log.Printf("- Unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if r.Method == "HEAD" {
		w.WriteHeader(200)
		return
	}
	if r.Method == "GET" {
		// Return the uptime. This is a small enough information leak.
		io.WriteString(w, time.Since(start).String())
		return
	}
	if r.Method != "POST" {
		http.Error(w, "Invalid method", http.StatusMethodNotAllowed)
		log.Printf("- invalid method %s", r.Method)
		return
	}
	payload, err := github.ValidatePayload(r, []byte(s.WebHookSecret))
	if err != nil {
		http.Error(w, "Invalid secret", http.StatusUnauthorized)
		log.Printf("- invalid secret")
		return
	}
	if t := github.WebHookType(r); t != "ping" {
		event, err := github.ParseWebHook(t, payload)
		if err != nil {
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			log.Printf("- invalid payload")
			return
		}
		// Process the rest asynchronously so the hook doesn't take too long.
		switch event := event.(type) {
		case *github.PushEvent:
			if event.HeadCommit == nil {
				log.Printf("- Push %s %s <deleted>", *event.Repo.FullName, *event.Ref)
			} else {
				log.Printf("- Push %s %s %s", *event.Repo.FullName, *event.Ref, *event.HeadCommit.ID)
				pullRepo()
			}
		default:
			log.Printf("- ignoring hook type %s", reflect.TypeOf(event).Elem().Name())
		}
	}
	io.WriteString(w, "{}")
}

func mainImpl() error {
	start = time.Now()
	port := flag.Int("port", 0, "port to use")
	secret := flag.String("secret", "", "secret to use")
	flag.Parse()
	if runtime.GOOS != "windows" {
		log.SetFlags(0)
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	s := server{WebHookSecret: *secret}
	// Run the web server.
	http.Handle("/", &s)
	thisFile, err := osext.Executable()
	if err != nil {
		return err
	}
	log.Printf("Running in: %s", wd)
	log.Printf("Executable: %s", thisFile)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return err
	}
	a := ln.Addr().String()
	ln.Close()
	log.Printf("Listening on: %s", a)
	go http.ListenAndServe(a, nil)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Failed to initialize watcher: %v", err)
	} else if err = w.Add(thisFile); err != nil {
		log.Printf("Failed to initialize watcher: %v", err)
	}

	if err == nil {
		select {
		case <-w.Events:
		case err = <-w.Errors:
			log.Printf("Waiting failure: %v", err)
		}
	} else {
		// Hang so the server actually run.
		select {}
	}
	// Ensures no task is running.
	s.wg.Wait()
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "pullhook: %s.\n", err)
		os.Exit(1)
	}
}
