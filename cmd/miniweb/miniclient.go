// Copyright 2017-2021 National Technology & Engineering Solutions of Sandia, LLC (NTESS).
// Under the terms of Contract DE-NA0003525 with NTESS, the U.S. Government retains certain
// rights in this software.

package main

import (
	"strings"
	"sync"

	"github.com/sandia-minimega/minimega/v2/pkg/miniclient"
	log "github.com/sandia-minimega/minimega/v2/pkg/minilog"
)

var mmMu sync.Mutex
var mm *miniclient.Conn

// noOp returns a closed channel
func noOp() chan *miniclient.Response {
	log.Info("noop")
	out := make(chan *miniclient.Response)
	close(out)
	return out
}

// run minimega commands, automatically redialing if we were disconnected
func run(c *Command) chan *miniclient.Response {
	log.Info("miniclient run waiting for lock: %v", c.String())
	mmMu.Lock()
	defer mmMu.Unlock()
	defer log.Info("miniclient defer")

	var err error

	log.Info("Calling miniclient run: %v", c.String())

	if mm == nil {
		log.Info("Dialing")
		if mm, err = miniclient.Dial(*f_base); err != nil {
			log.Error("unable to dial: %v", err)
			return noOp()
		}
	}

	// check if there's already an error and try to redial
	if err := mm.Error(); err != nil {
		s := err.Error()
		log.Debug("miniclient saw error: %v", s)
		if strings.Contains(s, "broken pipe") || strings.Contains(s, "no such file or directory") || strings.Contains(s, "requester disconnected") {
			log.Info("Redialing")
			if mm, err = miniclient.Dial(*f_base); err != nil {
				log.Error("unable to redial: %v", err)
				return noOp()
			}
		} else if !strings.Contains(s, "requester disconnected") {
			return noOp()
		}
	}

	log.Info("running: %v", c)
	return mm.Run(c.String())
}
