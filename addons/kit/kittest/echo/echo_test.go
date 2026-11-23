package echo_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/kittest"
	"github.com/agentkourier/agent-kourier/addons/kit/kittest/echo"
)

// The kit runs its reference add-on on Postgres; the add-ons run it on SQLite, under the drivers they link.
func TestEchoOnPostgres(t *testing.T) { kittest.RunPostgres(t, echo.Subject()) }
