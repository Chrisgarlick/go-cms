// Package version is set at build time with -ldflags (spec 9.3).
package version

import (
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
	// Repo is the GitHub "owner/name" of the cms repository; site repos call its shared workflow.
	Repo = "Chrisgarlick/go-cms"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if i.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					i.Commit = s.Value
				case "vcs.time":
					i.Date = s.Value
				}
			}
		}
	}
	return i
}
