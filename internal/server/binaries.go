package server

import (
	"net/http"
	"path/filepath"
)

// DaemonBinaryPath, followed by "amd64" or "arm64", is where the server
// serves the daemon binary for Linux on that architecture.
const DaemonBinaryPath = "/daemon/linux-"

// daemonArchitectures are the architectures a provisioned VPS can have.
var daemonArchitectures = []string{"amd64", "arm64"}

// routeDaemonBinaries serves dir's daemon-linux-<arch> at
// DaemonBinaryPath<arch>, without authentication: a VPS downloads it
// before it has a certificate, and the binary is public material. A
// missing file is answered 404.
func (s *Server) routeDaemonBinaries(dir string) {
	for _, arch := range daemonArchitectures {
		file := filepath.Join(dir, "daemon-linux-"+arch)
		s.mux.HandleFunc("GET "+DaemonBinaryPath+arch, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, file)
		})
	}
}
