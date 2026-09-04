// Copyright Armada Contributors

package main

import (
	"cmp"
	"context"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/armadakv/armada/version"
	"github.com/urfave/cli/v3"
)

var versionCmd = &cli.Command{
	Name:  "version",
	Usage: "Print current version.",
	Action: func(ctx context.Context, c *cli.Command) error {
		additional := ""
		info, ok := debug.ReadBuildInfo()
		if ok {
			sorted := slices.Clone(info.Settings)
			slices.SortFunc(sorted, func(a, b debug.BuildSetting) int { return cmp.Compare(a.Key, b.Key) })
			additional = fmt.Sprintf("Go: %s (%s/%s)\n", info.GoVersion, getBuildSetting(sorted, "GOOS"), getBuildSetting(sorted, "GOARCH"))
		}
		fmt.Printf("Copyright Armada Contributors\n\nArmada arq\nVersion: %s\n%s", version.Version, additional)
		return nil
	},
}

func getBuildSetting(settings []debug.BuildSetting, name string) string {
	if idx, found := slices.BinarySearchFunc(settings, name, func(setting debug.BuildSetting, s string) int { return cmp.Compare(setting.Key, s) }); found {
		return settings[idx].Value
	}
	return ""
}
