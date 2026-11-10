package slack_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The errors of the adapter reach the log as the cause of "slack connection ended; connecting again", where they go through the redaction,
// which reads a secret word followed by a colon as a name and its value. "slack auth.test: invalid_auth" has the word "auth" in it, and showed
// as "slack auth.test: [redacted]", which hid the code that says why. The prefix of such an error ends in a word that is no secret name.
func TestTheErrorsOfRunSurviveRedaction(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("Run succeeded")
		}
		if got := redact.FreeText(err.Error()); got != err.Error() {
			t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
		}
	}
	for _, code := range []string{"invalid_auth", "token_revoked", "account_inactive", "ratelimited", "internal_error"} {
		t.Run("auth.test answers "+code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
			}))
			defer srv.Close()
			cfg := clientConfig(newFake(t))
			cfg.APIURL = srv.URL + "/"
			err := slack.New(cfg).Run(t.Context(), nil)
			check(t, err)
			if !strings.Contains(err.Error(), code) {
				t.Errorf("Run = %v; want the code Slack answered", err)
			}
		})
	}
	t.Run("auth.test gives no bot user", func(t *testing.T) {
		check(t, slack.New(clientConfig(newFake(t, fakeslack.WithBot("", "BFAKEBOT", "AFAKEAPP")))).Run(t.Context(), &sink{}))
	})
}

// No message the adapter makes ends a prefix in a secret name and a colon. This reads the adapter's source: the name of each call that
// scrub words ("slack <method>: <cause>"), and every message that begins "slack ", with a placeholder where a verb is, must come out of the
// redaction as it went in. A new method whose name holds a secret word (auth, token, secret, key, session and the like) fails here.
func TestEveryPrefixOfTheAdaptersErrorsSurvivesRedaction(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	verb := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	var seen int
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			var texts []string
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "scrub" && len(n.Args) == 2 {
					if lit, ok := n.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						op, _ := strconv.Unquote(lit.Value)
						texts = append(texts, "slack "+op+": rate_limited")
					}
				}
			case *ast.BasicLit:
				if s, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING && strings.HasPrefix(s, "slack ") {
					texts = append(texts, verb.ReplaceAllString(s, "x"))
				}
			}
			for _, text := range texts {
				seen++
				if got := redact.FreeText(text); got != text {
					t.Errorf("%s: redaction changes %q to %q", fset.Position(n.Pos()), text, got)
				}
			}
			return true
		})
	}
	if seen < 20 {
		t.Errorf("found %d prefixes in the source; want the scan to find the adapter's calls", seen)
	}
}
