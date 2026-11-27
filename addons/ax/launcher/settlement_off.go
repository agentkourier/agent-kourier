//go:build !testhooks

package launcher

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func beforeSettlement(context.Context, *a2a.Task) bool { return true }
