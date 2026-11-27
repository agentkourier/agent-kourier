// Package result defines the wire protocol between the AX runner and the launcher.
package result

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Path and StartPath are the two callback routes. The environment names are reserved in AX Tasks.
const (
	Path        = "/ax/results/v1"
	StartPath   = Path + "/start"
	URLEnv      = "AGENT_KOURIER_AX_RESULT_URL"
	IDEnv       = "AGENT_KOURIER_AX_RESULT_ID"
	DeadlineEnv = "AGENT_KOURIER_AX_RESULT_DEADLINE"
	OutputLimit = 64 << 10
	BodyLimit   = 400 << 10
)

// Claim identifies the wrapper invocation seeking permission to run.
type Claim struct {
	ResultID     string `json:"result_id"`
	TaskUID      string `json:"task_uid"`
	InvocationID string `json:"invocation_id"`
}

// Report carries one command outcome.
type Report struct {
	Claim
	ExitCode     int    `json:"exit_code"`
	Output       string `json:"output"`
	OutputSource string `json:"output_source"`
	Truncated    bool   `json:"truncated"`
}

// NewID returns a random 128-bit ID encoded as lowercase hex.
func NewID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func validID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Valid checks the required claim identifiers.
func (c Claim) Valid() bool {
	return validID(c.ResultID) && validID(c.InvocationID) && strings.TrimSpace(c.TaskUID) != "" && len(c.TaskUID) <= 128
}

// URL validates the HTTPS result endpoint.
func URL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.EscapedPath() != Path || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, errors.New("invalid HTTPS result URL")
	}
	return u, nil
}

// Decode accepts exactly the fields of the chosen callback route.
func Decode(body []byte, start bool) (Report, error) {
	invalid := errors.New("invalid callback object")
	if !utf8.Valid(body) {
		return Report{}, invalid
	}
	var report Report
	fields := map[string]any{
		"result_id": &report.ResultID, "task_uid": &report.TaskUID, "invocation_id": &report.InvocationID,
	}
	if !start {
		fields["exit_code"] = &report.ExitCode
		fields["output"] = &report.Output
		fields["output_source"] = &report.OutputSource
		fields["truncated"] = &report.Truncated
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return Report{}, invalid
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return Report{}, invalid
		}
		name, ok := key.(string)
		if !ok || fields[name] == nil {
			return Report{}, invalid
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, fields[name]) != nil {
			return Report{}, invalid
		}
		delete(fields, name)
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') || len(fields) != 0 || !report.Valid() {
		return Report{}, invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return Report{}, invalid
	}
	if start {
		return report, nil
	}
	if report.ExitCode < 0 || report.ExitCode > 255 || len(report.Output) > OutputLimit {
		return Report{}, invalid
	}
	switch report.OutputSource {
	case "file", "stdout_stderr":
	case "none":
		if report.Output != "" {
			return Report{}, invalid
		}
	default:
		return Report{}, invalid
	}
	return report, nil
}
