// Copyright Armada Contributors

package main

import (
	"cmp"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/armadakv/armada/version"
	"github.com/urfave/cli/v2"
)

var versionCmd = &cli.Command{
	Name:  "version",
	Usage: "Print current version.",
	Action: func(c *cli.Context) error {
		additional := ""
		info, ok := debug.ReadBuildInfo()
		if ok {
			additional = fmt.Sprintf("Go: %s (%s/%s)\n", info.GoVersion, getBuildSetting(info.Settings, "GOOS"), getBuildSetting(info.Settings, "GOARCH"))
		}
		fmt.Printf("Copyright Armada Contributors\n\nArmada arctl\nVersion: %s\n%s", version.Version, additional)
		return nil
	},
}

func getBuildSetting(settings []debug.BuildSetting, name string) string {
	if idx, found := slices.BinarySearchFunc(settings, name, func(setting debug.BuildSetting, s string) int { return cmp.Compare(setting.Key, s) }); found {
		return settings[idx].Value
	}
	return ""
}
