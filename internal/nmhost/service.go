// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"lectureflow/internal/bridge"
)

// Service is the app's whole Native Messaging side: the endpoint, the
// server on it and the registration (cmd starts it; the API reports it).
type Service struct {
	srv *Server
	reg *Registrar // nil: this instance does not manage the registration
	log *slog.Logger

	mu        sync.Mutex
	ln        net.Listener
	listenErr string
	served    chan struct{}

	// The periodic AutoRegister (macOS): stop ends it, watched is closed
	// when it has ended.
	stop     chan struct{}
	stopOnce sync.Once
	watched  chan struct{}
}

// ServiceOptions configure StartService.
type ServiceOptions struct {
	// Endpoint to listen on; "" = off.
	Endpoint string
	Handler  bridge.Handler
	// Registrar is nil when this instance must not touch the browsers'
	// registration (an overridden endpoint, tests).
	Registrar *Registrar
	// RegisterOnStart runs Registrar.Register in the background.
	RegisterOnStart bool
	Log             *slog.Logger
	// Server adjusts the server's options (tests).
	Server func(*ServerOptions)
	// WatchEvery is how often the service looks for a browser to register
	// by itself (Registrar.AutoRegister; macOS, where Chrome and Edge
	// create their NativeMessagingHosts directory at their first start):
	// 0 = DefaultWatchEvery, negative = never. Only a registrar that
	// writes into the browsers' directories is watched.
	WatchEvery time.Duration
}

// DefaultWatchEvery is ServiceOptions.WatchEvery's default: a browser
// opened for the first time while LectureFlow runs is registered within
// this time (the user is still installing the extension).
const DefaultWatchEvery = 30 * time.Second

// Listen-error values of Status.ListenError.
const (
	ListenOff    = "off"
	ListenInUse  = "in_use"
	ListenFailed = "failed"
)

// ErrNotManaged: this instance does not manage the registration.
var ErrNotManaged = errors.New("nmhost: this instance does not manage the browser registration")

// StartService listens on the endpoint and serves it in the background. A
// failure to listen is logged and reported by Status (ListenError); the
// app runs on without it. An endpoint another program holds (a second
// LectureFlow, or one that took the name first: ErrInUse) is ListenInUse:
// the extension cannot reach this LectureFlow, and nothing says it can.
func StartService(o ServiceOptions) *Service {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	so := ServerOptions{Handler: o.Handler, Log: log}
	if o.Server != nil {
		o.Server(&so)
	}
	s := &Service{srv: NewServer(so), reg: o.Registrar, log: log}
	switch ln, err := listenOn(o.Endpoint); {
	case o.Endpoint == "":
		s.listenErr = ListenOff
		log.Info("nm: browser extension endpoint is off")
	case err != nil:
		s.listenErr = ListenFailed
		if errors.Is(err, ErrInUse) {
			s.listenErr = ListenInUse
			log.Warn("nm: the browser extension endpoint is served by another process; the extension cannot reach this LectureFlow", "endpoint", o.Endpoint)
		} else {
			log.Warn("nm: cannot serve the browser extension endpoint", "endpoint", o.Endpoint, "err", err)
		}
	default:
		s.ln = ln
		s.served = make(chan struct{})
		go func() {
			defer close(s.served)
			if err := s.srv.Serve(ln); err != nil {
				log.Warn("nm: serving stopped", "err", err)
			}
		}()
		log.Info("nm: serving the browser extension endpoint", "endpoint", o.Endpoint)
	}
	if s.reg != nil && o.RegisterOnStart {
		go func() {
			res, err := s.reg.Register()
			switch {
			case errors.Is(err, ErrHostMissing):
				log.Info("nm: browser registration skipped: no host program in this build")
			case err != nil:
				log.Warn("nm: browser registration failed", "err", err)
			case len(res.Notes) > 0:
				log.Warn("nm: browser registration incomplete", "notes", res.Notes)
			default:
				log.Info("nm: browsers registered")
			}
		}()
	}
	every := o.WatchEvery
	if every == 0 {
		every = DefaultWatchEvery
	}
	if s.reg != nil && s.reg.watches() && every > 0 {
		s.stop, s.watched = make(chan struct{}), make(chan struct{})
		go s.watch(every)
	}
	return s
}

// watch runs Registrar.AutoRegister every period until Close.
func (s *Service) watch(every time.Duration) {
	defer close(s.watched)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if s.reg.AutoRegister() {
				s.log.Info("nm: registered again: a browser has created its native messaging directory")
			}
		}
	}
}

func listenOn(endpoint string) (net.Listener, error) {
	if endpoint == "" {
		return nil, nil
	}
	return Listen(endpoint)
}

// Server is the bridge server (LastHello).
func (s *Service) Server() *Server { return s.srv }

// Status reports the registration, the endpoint and the latest hello.
func (s *Service) Status() Status {
	var st Status
	if s.reg != nil {
		st = s.reg.Status()
	} else {
		st.Browsers = []BrowserStatus{}
	}
	st.Managed = s.reg != nil
	s.mu.Lock()
	st.Listening = s.ln != nil
	st.ListenError = s.listenErr
	s.mu.Unlock()
	if h, ok := s.srv.LastHello(); ok {
		st.LastConnection = &h
	}
	return st
}

// Register runs the registration again ("重新登记") and returns the new
// status. ErrNotManaged when this instance does not manage it.
func (s *Service) Register() (Status, error) {
	if s.reg == nil {
		return s.Status(), ErrNotManaged
	}
	_, err := s.reg.Register()
	return s.Status(), err
}

// Close stops serving and ends every connection.
func (s *Service) Close() {
	if s.stop != nil {
		s.stopOnce.Do(func() { close(s.stop) })
		<-s.watched
	}
	s.srv.Close()
	s.mu.Lock()
	ln, served := s.ln, s.served
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
		<-served
	}
}
