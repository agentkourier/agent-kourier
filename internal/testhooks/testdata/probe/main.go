// Command probe calls each test hook once, so a test can build it with and without the testhooks tag
// and inspect the binary.
package main

import (
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

func main() {
	fmt.Printf("slack=%q\n", testhooks.SlackAPIURL())
	testhooks.AfterPost()
	fmt.Println("after-post-returned")
}
