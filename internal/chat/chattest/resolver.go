package chattest

import "github.com/agentkourier/agent-kourier/internal/chat"

// Same is a chat.Resolver that answers every connection, the empty one too, with p: for a test of a part that runs one
// connection, whatever it is called. A test of the connection a call reaches uses chat.PlatformSet, which answers only the
// connections it holds.
func Same[P any](p P) chat.Resolver[P] { return same[P]{p} }

type same[P any] struct{ p P }

func (s same[P]) Platform(string) (P, error) { return s.p, nil }
