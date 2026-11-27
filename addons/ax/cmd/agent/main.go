// Command agent runs the Agent Kourier AX Task launcher.
package main

import (
	"github.com/agentkourier/agent-kourier/addons/ax/launcher"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

// version is set by the release build.
var version = "dev"

func main() { runtime.Main(launcher.Addon(version)) }
