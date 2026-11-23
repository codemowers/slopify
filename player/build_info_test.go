package main

import "testing"

func TestPlayerBuildMetric(t *testing.T) {
	families, err := nativeMetricsRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "slopify_player_build_info" {
			continue
		}
		if len(family.Metric) != 1 || family.Metric[0].GetGauge().GetValue() != 1 {
			t.Fatalf("expected exactly one info sample with value 1: %v", family)
		}
		labels := map[string]string{}
		for _, label := range family.Metric[0].Label {
			labels[label.GetName()] = label.GetValue()
		}
		for key, want := range map[string]string{
			"version": playerBuild.Version, "revision": playerBuild.Revision,
			"modified": playerBuild.Modified, "build_time": playerBuild.BuildTime,
			"commit_time": playerBuild.CommitTime, "go_version": playerBuild.GoVersion,
		} {
			if want == "" || labels[key] != want {
				t.Errorf("%s = %q, want nonempty %q", key, labels[key], want)
			}
		}
		return
	}
	t.Fatal("build info metric is missing from the player registry")
}

func TestPlayerBuildLinkerOverrides(t *testing.T) {
	oldVersion, oldTime, oldRevision := buildVersion, buildTime, buildRevision
	t.Cleanup(func() { buildVersion, buildTime, buildRevision = oldVersion, oldTime, oldRevision })
	buildVersion, buildTime, buildRevision = "v1.2.3", "2026-11-18T15:00:00Z", "0123456789abcdef"
	b := readPlayerBuild()
	if b.Version != buildVersion || b.BuildTime != buildTime || b.Revision != buildRevision {
		t.Fatalf("linker metadata not preserved: %+v", b)
	}
}
