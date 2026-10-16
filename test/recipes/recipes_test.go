// Package recipes_test checks the recipes under examples/recipes: each recipe's kourier/ directory
// must load with the broker's own loader, and its Bindings must resolve to the AgentBackend and
// ChatConnection that sit beside them.
package recipes_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// recipesDir is the directory the recipes live in, relative to this package.
const recipesDir = "../../examples/recipes"

// TestRecipes checks every recipe under examples/recipes. With none yet it passes and says so, so
// a green run is not mistaken for coverage.
func TestRecipes(t *testing.T) {
	recipes, err := recipeDirs(recipesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipes) == 0 {
		t.Logf("no recipes found under %s: nothing to check", recipesDir)
		return
	}
	t.Logf("checking %d recipes under %s", len(recipes), recipesDir)
	for _, dir := range recipes {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ran, err := checkRecipe(t.Context(), dir)
			if err != nil {
				t.Error(err)
			}
			// A recipe may have no trigger evals (one that starts only from mentions has no botMessage
			// trigger to test), so their absence is logged, not failed.
			if !ran && err == nil {
				t.Logf("%s has no %s: its triggers are not evaluated", filepath.Base(dir), triggersFile)
			}
		})
	}
}

// TestCheckRecipe runs the checker on fixtures: one recipe it must accept, and recipes it must refuse,
// each with a message that names the file at fault.
func TestCheckRecipe(t *testing.T) {
	tests := []struct {
		name string
		want []string // substrings the error must contain; none means the recipe must pass
	}{
		{name: "valid"},
		{name: "bad-field", want: []string{"kourier/binding.yaml#1 Binding recipe/helper", "spec.chat.chanel: unknown field"}},
		{name: "unresolved-backend", want: []string{"kourier/binding.yaml", "Binding recipe/helper", "spec.agent.backendRef", "AgentBackend recipe/agnts not found"}},
		{name: "unresolved-connection", want: []string{"kourier/binding.yaml", "Binding recipe/helper", "spec.chat.connectionRef", "ChatConnection recipe/slak not found"}},
		{name: "wrong-apiversion", want: []string{"kourier/agent.yaml#1 Agent recipe/helper", `apiVersion "kagent.dev/v1alpha2" is not supported`}},
		{name: "no-binding", want: []string{"kourier", "no Binding"}},
		{name: "no-kourier-dir", want: []string{"no-kourier-dir", "kourier directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkRecipe(t.Context(), filepath.Join("testdata", tt.name))
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("checkRecipe: %v; want it to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkRecipe passed; want an error containing %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(filepath.ToSlash(err.Error()), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// TestCheckRecipeNamesEachObjectsOwnFile pins the mapping from a reference problem to a file: a file of
// several documents, and a Binding of the same name in another namespace in another file, each with a
// broken reference of its own.
func TestCheckRecipeNamesEachObjectsOwnFile(t *testing.T) {
	_, err := checkRecipe(t.Context(), filepath.Join("testdata", "two-files"))
	if err == nil {
		t.Fatal("checkRecipe passed; want the two broken Bindings reported")
	}
	var seen int
	for _, line := range strings.Split(filepath.ToSlash(err.Error()), "\n") {
		var file, other string
		switch {
		case strings.Contains(line, "Binding recipe/helper:"):
			file, other = "kourier/bindings.yaml: Binding recipe/helper", "other.yaml"
		case strings.Contains(line, "Binding other/helper:"):
			file, other = "kourier/other.yaml: Binding other/helper", "bindings.yaml"
		default:
			t.Errorf("unexpected line %q", line)
			continue
		}
		seen++
		if !strings.Contains(line, file) || strings.Contains(line, other) {
			t.Errorf("line %q should name %s and not %s", line, file, other)
		}
	}
	if seen != 3 { // recipe/helper's backend, other/helper's backend and connection
		t.Errorf("%d problems reported, want 3:\n%v", seen, err)
	}
	if strings.Contains(err.Error(), "Binding recipe/first") {
		t.Errorf("the valid Binding is reported: %v", err)
	}
}

// TestLocateDoesNotInventAPath requires a problem about an object whose file is not known to say so and
// to name no file.
func TestLocateDoesNotInventAPath(t *testing.T) {
	problems := errors.Join(
		errors.New("Binding recipe/known: spec.agent.backendRef: not found"),
		errors.New("ChatConnection recipe/slack: spec.credentialsSecretRef: Secret recipe/slack not found"),
	)
	got := locate("r/kourier", problems, map[string]string{"Binding recipe/known": "binding.yaml"}).Error()
	want := "r/kourier/binding.yaml: Binding recipe/known: spec.agent.backendRef: not found\n" +
		"r/kourier: ChatConnection recipe/slack: spec.credentialsSecretRef: Secret recipe/slack not found (file not found for this object)"
	if filepath.ToSlash(got) != want {
		t.Errorf("locate =\n%s\nwant\n%s", got, want)
	}
}

// TestCheckRecipeReportsAStatFailureAsIs requires a directory that cannot be examined to be reported with its
// own error, not as a missing kourier directory.
func TestCheckRecipeReportsAStatFailureAsIs(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := checkRecipe(t.Context(), notADir) // notADir/kourier fails with ENOTDIR
	if err == nil || strings.Contains(err.Error(), "needs a kourier directory") {
		t.Errorf("checkRecipe = %v, want the stat error", err)
	}
}

// TestUnderDir requires a problem to get a path only if Load started it with a file of the directory.
func TestUnderDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	problems := errors.Join(
		errors.New("binding.yaml#2 Binding a/b: spec.chat.channel: required"),
		errors.New("binding.yaml: read: file too large"),
		errors.New("read config directory: permission denied"),
		fmt.Errorf("load config: %w", context.Canceled),
		errors.New("other.yaml#1 Binding a/c: not a file of the directory"),
	)
	want := []string{
		filepath.Join(dir, "binding.yaml") + "#2 Binding a/b: spec.chat.channel: required",
		filepath.Join(dir, "binding.yaml") + ": read: file too large",
		dir + ": read config directory: permission denied",
		dir + ": load config: context canceled",
		dir + ": other.yaml#1 Binding a/c: not a file of the directory",
	}
	got := underDir(dir, problems)
	if !errors.Is(got, context.Canceled) {
		t.Errorf("underDir loses the wrapped error: %v", got)
	}
	if lines := strings.Split(got.Error(), "\n"); !slices.Equal(lines, want) {
		t.Errorf("underDir =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if single := underDir(dir, errors.New("read config directory: x")); single.Error() != dir+": read config directory: x" {
		t.Errorf("underDir of a single problem = %q", single)
	}
}

// TestCheckRecipeReportsAKourierProblemOnce requires a kourier/ that does not load to be reported once, in
// the file that has it, with the evals not run and no problem blamed on the evals file.
func TestCheckRecipeReportsAKourierProblemOnce(t *testing.T) {
	ran, err := checkRecipe(t.Context(), filepath.Join("testdata", "evals-bad-kourier"))
	if err == nil {
		t.Fatal("checkRecipe passed; want the kourier problem")
	}
	msg := filepath.ToSlash(err.Error())
	if ran {
		t.Error("the evals ran over a kourier directory that did not load")
	}
	if n := strings.Count(msg, "spec.chat.chanel: unknown field"); n != 1 {
		t.Errorf("the kourier problem is reported %d times, want once:\n%s", n, msg)
	}
	for _, want := range []string{"evals-bad-kourier/kourier/binding.yaml#1 Binding recipe/alerts", "trigger evals not run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "triggers.yaml") {
		t.Errorf("a problem is blamed on the evals file:\n%s", msg)
	}
}

// TestRecipeDirs pins which entries of the recipes directory count as recipes.
func TestRecipeDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"b-recipe", "a-recipe", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("index\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := recipeDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "a-recipe"), filepath.Join(root, "b-recipe")}
	if !slices.Equal(got, want) {
		t.Errorf("recipeDirs = %v, want %v", got, want)
	}
	if _, err := recipeDirs(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("recipeDirs of a missing directory = %v, want one wrapping fs.ErrNotExist", err)
	}
}

// recipeDirs returns the recipe directories directly inside root, sorted: every directory whose name
// does not start with a dot. Files such as the index README are not recipes.
func recipeDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read recipes directory: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}

// checkRecipe checks the recipe at dir, loading its kourier/ directory once. The directory must load with
// config.Load and hold at least one Binding; its trigger evals, if it has any, must pass over the Bindings
// loaded; and the Bindings must resolve with config.Resolve, the broker's own reference check. Every problem is
// reported, joined, and each names the file it comes from. When kourier/ does not load, its problems are reported
// alone, once, and the evals are not run. ran says the evals were run.
func checkRecipe(ctx context.Context, dir string) (ran bool, err error) {
	kourier := filepath.Join(dir, "kourier")
	set, err := loadKourier(ctx, dir)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, triggersFile)); statErr == nil {
			err = errors.Join(err, fmt.Errorf("%s: trigger evals not run: the kourier directory did not load", dir))
		}
		return false, err
	}
	files, err := bindingFiles(kourier)
	if err != nil {
		return false, err
	}
	// The evals only read the Bindings, and run first because Resolve takes ownership of set. Load does not
	// accept a Secret, so a recipe cannot ship one: the operator creates them, and the resolver reports each
	// as present, which leaves Resolve only the references between the recipe's own objects to find.
	ran, evalErr := checkTriggerEvals(dir, set)
	var resolveErr error
	if _, err := config.Resolve(ctx, set, config.PresentSecrets(set)); err != nil {
		resolveErr = locate(kourier, err, files)
	}
	return ran, errors.Join(resolveErr, evalErr)
}

// loadKourier loads the kourier/ directory of the recipe at dir with config.Load and requires a Binding in it.
func loadKourier(ctx context.Context, dir string) (*config.Set, error) {
	kourier := filepath.Join(dir, "kourier")
	info, err := os.Stat(kourier)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("check recipe %s: %w", dir, err)
	case err != nil || !info.IsDir():
		return nil, fmt.Errorf("%s: a recipe needs a kourier directory holding its Agent Kourier resources", dir)
	}
	set, err := config.Load(ctx, kourier)
	if err != nil {
		return nil, underDir(kourier, err)
	}
	if len(set.Bindings) == 0 {
		return nil, fmt.Errorf("%s: no Binding found; a recipe ships at least one", kourier)
	}
	return set, nil
}

// bindingFiles maps "Binding namespace/name" to the file that defines it, so a reference problem,
// which Resolve reports by object, can say which file to open.
func bindingFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read recipe directory: %w", err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); e.IsDir() || strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read recipe file: %w", err)
		}
		r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for {
			doc, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			var m metav1.PartialObjectMetadata
			if err := yaml.Unmarshal(doc, &m); err == nil && m.Kind == "Binding" {
				files[fmt.Sprintf("Binding %s/%s", m.Namespace, m.Name)] = e.Name()
			}
		}
	}
	return files, nil
}

// underDir reports each problem of err, which config.Load returned for the directory dir. Load starts a
// problem with the file it is about ("<file>#<n> ..." or "<file>: ..."): those get the directory put before the
// file name, so the path printed is one a reader can open. Any other problem, such as a directory that cannot
// be read or a cancelled context, is not about a file and gets only "<dir>: ", not an invented path.
func underDir(dir string, err error) error {
	var names []string
	if entries, readErr := os.ReadDir(dir); readErr == nil {
		for _, e := range entries {
			names = append(names, e.Name())
		}
	}
	return mapEach(err, func(e error) error {
		for _, n := range names {
			if strings.HasPrefix(e.Error(), n+"#") || strings.HasPrefix(e.Error(), n+":") {
				return fmt.Errorf("%s%c%w", dir, filepath.Separator, e)
			}
		}
		return fmt.Errorf("%s: %w", dir, e)
	})
}

// locate prefixes each problem of err, which Resolve reports as "<kind> <namespace>/<name>: ...", with the
// file in dir that defines the object, taken from files. A problem it cannot place is not given a path: it
// is marked instead.
func locate(dir string, err error, files map[string]string) error {
	return mapEach(err, func(e error) error {
		for object, file := range files {
			if strings.HasPrefix(e.Error(), object+":") {
				return fmt.Errorf("%s: %w", filepath.Join(dir, file), e)
			}
		}
		return fmt.Errorf("%s: %w (file not found for this object)", dir, e)
	})
}

// mapEach applies f to each problem of err, or to err itself when it is not a join, and joins the results.
func mapEach(err error, f func(error) error) error {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return f(err)
	}
	var out []error
	for _, e := range joined.Unwrap() {
		out = append(out, f(e))
	}
	return errors.Join(out...)
}
