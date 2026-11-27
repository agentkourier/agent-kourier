// Command probe calls each test hook once, so a test can build it with and without the testhooks tag
// and inspect the binary.
package main

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

func main() {
	fmt.Printf("slack=%q\n", testhooks.SlackAPIURL(testhooks.Overrides{}))
	teams, _, err := testhooks.Teams(testhooks.Overrides{})
	fmt.Printf("teams=%q err=%v\n", teams.GraphURL, err)
	fmt.Printf("watchdog=%s\n", testhooks.AgentStreamTimeout())
	_ = testhooks.BeforeSend(context.Background(), "probe")
	testhooks.AfterPost()
	fmt.Println("after-post-returned")
}
