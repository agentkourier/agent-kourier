package store

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// encodePageToken is the cursor after a task: its update time and ID.
func encodePageToken(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + "_" + id))
}

func decodePageToken(token string) (at int64, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	n, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	at, err = strconv.ParseInt(n, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("page token: %w", a2a.ErrInvalidParams)
	}
	return at, id, nil
}
