// Copyright (c) 2021 Arista Networks, Inc.  All rights reserved.
// Arista Networks, Inc. Confidential and Proprietary.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/utils/inotify"
)

type Event struct {
	Count uint  `json:"count"`
	Last  int64 `json:"last"`
}

type Events struct {
	Event map[string]Event `json:"event,omitempty"`
	Paths []string         `json:"paths,omitempty"`
}

func saveCache(events Events, cache string) error {
	tempDir := filepath.Dir(cache)
	fp, err := ioutil.TempFile(tempDir, "tmp.execwatch*.json")
	if err != nil {
		if out, err2 := json.MarshalIndent(&events, "\n", " "); err2 == nil {
			fmt.Println("cache:", string(out))
		}
		return fmt.Errorf("unable to create tempfile: %w", err)
	}
	defer fp.Close()
	e := json.NewEncoder(fp)
	if err := e.Encode(&events); err != nil {
		return fmt.Errorf("unable to encode JSON for cache: %w", err)
	}
	defer func() {
		fp.Close()
		os.Remove(fp.Name())
	}()
	if err := os.Rename(fp.Name(), cache); err != nil {
		return fmt.Errorf("rename %q to %q: %w", fp.Name(), cache, err)
	}
	return nil
}

func newWatcher(paths []string) (*inotify.Watcher, error) {
	watcher, err := inotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	watchFlags := inotify.InDontFollow | inotify.InOpen
	for _, a := range paths {
		err := watcher.AddWatch(a, watchFlags)
		if err != nil {
			watcher.Close()
			return nil, fmt.Errorf("add watch for %q: %w", a, err)
		}
	}
	return watcher, nil
}

type lock struct {
	fd     *int
	locked bool
	path   string
}

func NewLockedLock(path string) (*lock, error) {
	lockPath := path + ".lock"
	mode := syscall.O_CREAT | syscall.O_RDWR
	var perm uint32
	perm = syscall.S_IRUSR | syscall.S_IWUSR
	fd, err := syscall.Open(lockPath, mode, perm)
	if err != nil {
		return nil, err
	}
	l := &lock{fd: &fd, path: lockPath}
	return l, l.Lock()
}

func (l *lock) Lock() error {
	if l == nil || l.fd == nil {
		return errors.New("not opened, use New")
	}
	if l.locked {
		return nil
	}
	err := syscall.Flock(*l.fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			return errors.New("already locked, is another instance already running?")
		}
		return err
	}
	l.locked = true
	return nil
}

func (l *lock) Unlock() {
	if l.fd == nil {
		return
	}
	syscall.Flock(*l.fd, syscall.LOCK_UN)
	syscall.Close(*l.fd)
	if err := syscall.Unlink(l.path); err != nil {
		log.Printf("unlink %q: %v", l.path, err)
	}
	l.fd = nil
}

type logWriter struct {
	pid *int
}

var pid = os.Getpid()

func (w logWriter) Write(bytes []byte) (int, error) {
	if w.pid == nil {
		pid := os.Getpid()
		w.pid = &pid
	}
	return fmt.Print("+", *w.pid, "+ ", time.Now().UTC().Format("2006-01-02T15:04:05.999Z")+" "+string(bytes))
}

func run() error {
	log.SetFlags(0)
	log.SetOutput(logWriter{})
	cache := os.Args[1]
	paths := os.Args[2:]
	if len(paths) == 0 {
		return fmt.Errorf("no paths to watch")
	}

	lock, err := NewLockedLock(cache)
	if err != nil {
		return fmt.Errorf("lock cache %q: %w", cache, err)
	}
	defer lock.Unlock()

	watcher, err := newWatcher(paths)
	if err != nil {
		return err
	}

	events := Events{
		Event: map[string]Event{},
		Paths: paths,
	}
	if fp, err := os.Open(cache); err == nil {
		d := json.NewDecoder(fp)
		if err := d.Decode(&events); err != nil {
			log.Println("warning: unable to read cache", cache, "", err)
		}
		fp.Close()
	}

	sigHdl := make(chan os.Signal)
	signal.Notify(sigHdl, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	dirty := false
	log.Printf("start watching %v", paths)
	for {
		select {
		case <-ticker.C:
			if dirty {
				if err := saveCache(events, cache); err != nil {
					log.Println("warning: unable to save cache", err)
				}
				dirty = false
			}

		case sig := <-sigHdl:
			log.Println("signal:", sig.String(), "save cache", cache)
			err := saveCache(events, cache)
			if err == nil {
				log.Println("saved cache:", cache)
			}
			return err

		case ev := <-watcher.Event:
			path, err := filepath.Abs(ev.Name)
			if err != nil {
				log.Println("warning:", err)
				path = ev.Name
			}
			info, err := os.Stat(path)
			if err == nil && !info.IsDir() {
				dirty = true
				event := events.Event[path]
				event.Count += 1
				event.Last = time.Now().UTC().UnixMilli()
				events.Event[path] = event
			}

		case err := <-watcher.Error:
			log.Println("watch error:", err)
			watcher.Close()
			if watcher, err = newWatcher(paths); err != nil {
				return err
			}
		}
	}
}

func main() {
	err := run()
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}
