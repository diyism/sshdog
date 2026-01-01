// Copyright 2016 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// TODO: High-level file comment.
package main

import (
	"embed"
	"fmt"
	"github.com/Matir/sshdog/daemon"
	"os"
	"strconv"
	"strings"
)

//go:embed config/*
var configFS embed.FS

type Debugger bool

func (d Debugger) Debug(format string, args ...interface{}) {
	if d {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "[DEBUG] %s\n", msg)
	}
}

var dbg Debugger = true

// Read file from embedded FS
func readConfigFile(name string) ([]byte, error) {
	return configFS.ReadFile("config/" + name)
}

// Just check if a file exists
func fileExists(name string) bool {
	_, err := readConfigFile(name)
	return err == nil
}

// Lookup the port number
func getPort() int16 {
	if len(os.Args) > 1 {
		if port, err := strconv.Atoi(os.Args[1]); err != nil {
			dbg.Debug("Error parsing %s as port: %v", os.Args[1], err)
		} else {
			return int16(port)
		}
	}
	if portData, err := readConfigFile("port"); err == nil {
		portStr := strings.TrimSpace(string(portData))
		if port, err := strconv.Atoi(portStr); err != nil {
			dbg.Debug("Error parsing %s as port: %v", portStr, err)
		} else {
			return int16(port)
		}
	}
	return 2222 // default
}

// Should we daemonize?
func shouldDaemonize() bool {
	return fileExists("daemon")
}

// Should we be silent?
func beQuiet() bool {
	return fileExists("quiet")
}

func main() {
	if beQuiet() {
		dbg = false
	}

	if shouldDaemonize() {
		if err := daemon.Daemonize(daemonStart); err != nil {
			dbg.Debug("Error daemonizing: %v", err)
		}
	} else {
		waitFunc, _ := daemonStart()
		if waitFunc != nil {
			waitFunc()
		}
	}
}

// Actually run the implementation of the daemon
func daemonStart() (waitFunc func(), stopFunc func()) {
	server := NewServer()

	// Always generate random host key at runtime for security
	// This prevents private key leakage if binary is compromised
	dbg.Debug("Generating random host key...")
	if err := server.RandomHostkey(); err != nil {
		dbg.Debug("Error generating random hostkey: %v", err)
		return
	}

	if authData, err := readConfigFile("authorized_keys"); err == nil {
		dbg.Debug("Adding authorized_keys.")
		server.AddAuthorizedKeys(authData)
	} else {
		dbg.Debug("No authorized keys found: %v", err)
		return
	}
	server.ListenAndServe(getPort())
	return server.Wait, server.Stop
}
