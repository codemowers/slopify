package main

import (
	"runtime"
	"runtime/debug"
)

// Set with -ldflags -X. Missing build metadata stays explicitly unknown.
var buildVersion = "dev"
var buildTime = "unknown"
var buildRevision = ""

type playerBuildInfo struct {
	Version    string `json:"version"`
	Revision   string `json:"revision"`
	Modified   string `json:"modified"`
	BuildTime  string `json:"buildTime"`
	CommitTime string `json:"commitTime"`
	GoVersion  string `json:"goVersion"`
}

var playerBuild = readPlayerBuild()

func readPlayerBuild() playerBuildInfo {
	b := playerBuildInfo{Version: buildVersion, Revision: "unknown", Modified: "unknown", BuildTime: buildTime, CommitTime: "unknown", GoVersion: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		if b.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			b.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				b.Revision = setting.Value
			case "vcs.modified":
				b.Modified = setting.Value
			case "vcs.time":
				b.CommitTime = setting.Value
			}
		}
	}
	if buildRevision != "" {
		b.Revision = buildRevision
	}
	return b
}
