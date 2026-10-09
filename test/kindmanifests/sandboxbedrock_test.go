package kindmanifests_test

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Bedrock provider of the sandbox: MODEL_PROVIDER=bedrock, a MODEL_NAME that is a model ID or an inference profile ID, a
// Bedrock API key as MODEL_API_KEY, and MODEL_REGION, which is required for bedrock and refused for the others. Secrets are
// the fake constants of sandboxenv_test.go.

const (
	// fakeProfile is an inference profile ID in the form the owner puts in MODEL_NAME: a geography prefix, the provider, the model,
	// a date and a version, with the dots, dashes and colon that a model name may hold.
	fakeProfile = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	fakeRegion  = "us-east-1"
)

// bedrockEnv is a file that passes for bedrock.
func bedrockEnv() []string {
	lines := withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER=bedrock")
	lines = withLine(lines, "MODEL_NAME", "MODEL_NAME="+fakeProfile)
	lines = withLine(lines, "MODEL_API_KEY", "MODEL_API_KEY="+fakeBedrockKey)
	return append(lines, "MODEL_REGION="+fakeRegion)
}

func TestEnvCheckAcceptsBedrock(t *testing.T) {
	for name, tc := range map[string]struct{ model, region string }{
		"an inference profile of a geography": {"us.anthropic.claude-haiku-4-5-20251001-v1:0", "us-east-1"},
		"a global inference profile":          {"global.anthropic.claude-sonnet-4-5-20250929-v1:0", "us-west-2"},
		"a bare model ID":                     {"anthropic.claude-3-haiku-20240307-v1:0", "eu-central-1"},
		"a region of three parts":             {fakeProfile, "ap-southeast-2"},
		"a GovCloud region":                   {fakeProfile, "us-gov-west-1"},
	} {
		t.Run(name, func(t *testing.T) {
			lines := withLine(bedrockEnv(), "MODEL_NAME", "MODEL_NAME="+tc.model)
			lines = withLine(lines, "MODEL_REGION", "MODEL_REGION="+tc.region)
			code, stdout, stderr := check(t, envFile(t, 0o600, lines...))
			if code != 0 || !strings.Contains(stdout, "MODEL_REGION") {
				t.Errorf("exit %d, stdout %q, stderr %q; want it accepted, naming MODEL_REGION", code, stdout, stderr)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

// TestEnvCheckNamesRegionOnlyForBedrock: the other providers' files do not have the key, and the output says so.
func TestEnvCheckNamesRegionOnlyForBedrock(t *testing.T) {
	_, stdout, _ := check(t, envFile(t, 0o600, goodEnv()...))
	if strings.Contains(stdout, "MODEL_REGION") {
		t.Errorf("stdout %q names MODEL_REGION for a provider that has none", stdout)
	}
}

func TestEnvCheckRefusesBedrockMistakes(t *testing.T) {
	bedrock := bedrockEnv()
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"no region":                       {withLine(bedrock, "MODEL_REGION", ""), "MODEL_REGION is missing"},
		"an empty region":                 {withLine(bedrock, "MODEL_REGION", "MODEL_REGION="), "MODEL_REGION is empty"},
		"a placeholder region":            {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=REPLACE"), "MODEL_REGION is still a placeholder"},
		"a region in capitals":            {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=US-EAST-1"), "MODEL_REGION is malformed"},
		"a region without its number":     {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=us-east"), "MODEL_REGION is malformed"},
		"a region with underscores":       {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=us_east_1"), "MODEL_REGION is malformed"},
		"a region with a two digit count": {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=us-east-10"), "MODEL_REGION is malformed"},
		"an availability zone":            {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=us-east-1a"), "MODEL_REGION is malformed"},
		"a host name":                     {withLine(bedrock, "MODEL_REGION", "MODEL_REGION=bedrock-runtime.us-east-1.amazonaws.com"), "MODEL_REGION is malformed"},
		"a region twice":                  {append(append([]string{}, bedrock...), "MODEL_REGION=us-west-2"), "MODEL_REGION is set twice"},
		"a region for anthropic":          {append(append([]string{}, goodEnv()...), "MODEL_REGION="+fakeRegion), "MODEL_REGION is only for MODEL_PROVIDER=bedrock"},
		"a region for openai":             {append(withLine(withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER=openai"), "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenAIKey), "MODEL_REGION="+fakeRegion), "MODEL_REGION is only for MODEL_PROVIDER=bedrock"},
		"a provider that is not known":    {withLine(bedrock, "MODEL_PROVIDER", "MODEL_PROVIDER=vertex"), "MODEL_PROVIDER is malformed"},
		"a profile ID with a slash":       {withLine(bedrock, "MODEL_NAME", "MODEL_NAME=us/anthropic.claude"), "MODEL_NAME is malformed"},
		"an ARN":                          {withLine(bedrock, "MODEL_NAME", "MODEL_NAME=arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude"), "MODEL_NAME is malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, tc.lines...))
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want a refusal that says %q", code, stderr, tc.want)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

// TestBedrockKeyGoesInTheSecretKeyKagentReads: the Bedrock API key reaches the Secret sandbox-model under the key
// AWS_BEARER_TOKEN_BEDROCK, which is the name kagent's Bedrock provider reads, as base64 on kubectl's stdin, with the same
// guarantees as the other secrets: on no argument list, in no environment (also with the region and every key exported in the
// caller's environment), and in no output.
func TestBedrockKeyGoesInTheSecretKeyKagentReads(t *testing.T) {
	bin, logs := recorders(t)
	script := `set -eu
. "$1"
K=(kubectl --context kind-agent-kourier-full)
sandbox_env_load "$2"
apply_secret kagent sandbox-model AWS_BEARER_TOKEN_BEDROCK=MODEL_API_KEY
printf '%s\n' "$SANDBOX_ENV_FOUND"
`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sandboxDir+"env.sh", envFile(t, 0o600, bedrockEnv()...))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLACK_BOT_TOKEN="+fakeBot, "SLACK_APP_TOKEN="+fakeApp, "SLACK_CHANNEL_ID="+fakeChannel, "MODEL_PROVIDER=bedrock", "MODEL_NAME="+fakeProfile,
		"MODEL_API_KEY="+fakeBedrockKey, "MODEL_REGION="+fakeRegion)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\nstdout %q\nstderr %q", err, out.String(), errOut.String())
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(fakeBedrockKey))
	stdin := readLog(t, logs, "kubectl.stdin")
	if !strings.Contains(stdin, "\n  AWS_BEARER_TOKEN_BEDROCK: "+encoded+"\n") || strings.Contains(stdin, "apiKey:") {
		t.Errorf("kubectl's stdin:\n%s\nwant the key under AWS_BEARER_TOKEN_BEDROCK and no apiKey", redactBase64(stdin))
	}
	for _, v := range []string{fakeBedrockKey, encoded} {
		for what, text := range map[string]string{"stdout": out.String(), "stderr": errOut.String(), "kubectl's argv": readLog(t, logs, "kubectl.argv"), "kubectl's env": readLog(t, logs, "kubectl.env"),
			"base64's argv": readLog(t, logs, "base64.argv"), "tr's argv": readLog(t, logs, "tr.argv")} {
			if strings.Contains(text, v) {
				t.Errorf("%s holds %q", what, v)
			}
		}
	}
	if strings.Contains(readLog(t, logs, "kubectl.stdin"), fakeBedrockKey) {
		t.Error("kubectl's stdin holds the key raw")
	}
	if !strings.Contains(out.String(), "MODEL_REGION") {
		t.Errorf("stdout %q: the loader did not report the region as found", out.String())
	}
}

// TestRenderFillsTheBedrockModelConfig: the loader's own render, as up.sh pipes it to kubectl, puts the model and the region in.
func TestRenderFillsTheBedrockModelConfig(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eu
. "$1"
MODEL_NAME="$3" MODEL_REGION="$4" SLACK_CHANNEL_ID=C0RENDER01 render "$2"`, "bash", sandboxDir+"env.sh", sandboxDir+"modelconfig-bedrock.yaml", fakeProfile, fakeRegion)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  provider: Bedrock\n", "  model: " + fakeProfile + "\n", "    region: " + fakeRegion + "\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("render gave\n%s\nwant %q in it", out, want)
		}
	}
	if strings.Contains(string(out), "__MODEL") {
		t.Error("a placeholder is left after rendering")
	}
}

// TestUpPicksTheBedrockSecretKey: up.sh makes the Bedrock Secret with the fixed key name for bedrock and apiKey for the
// others, and the ModelConfig it applies is the provider's own file.
func TestUpPicksTheBedrockSecretKey(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, `if [ "${MODEL_PROVIDER}" = bedrock ]; then`)
	if start < 0 {
		t.Fatal("up.sh has no if bedrock / else / fi around the model Secret")
	}
	elseAt := strings.Index(s[start+1:], "\nelse\n") + start + 1
	fiAt := strings.Index(s[elseAt:], "\nfi\n") + elseAt
	if elseAt <= start || fiAt <= elseAt {
		t.Fatal("up.sh has no if bedrock / else / fi around the model Secret")
	}
	applies := func(block string) (lines []string) {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "apply_secret ") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		return lines
	}
	if got := applies(s[start:elseAt]); len(got) != 1 || got[0] != "apply_secret kagent sandbox-model AWS_BEARER_TOKEN_BEDROCK=MODEL_API_KEY" {
		t.Errorf("the bedrock branch makes %q; want exactly one Secret for sandbox-model, under AWS_BEARER_TOKEN_BEDROCK", got)
	}
	if got := applies(s[elseAt:fiAt]); len(got) != 1 || got[0] != "apply_secret kagent sandbox-model apiKey=MODEL_API_KEY" {
		t.Errorf("the other branch makes %q; want exactly one Secret for sandbox-model, under apiKey", got)
	}
	if n := strings.Count(s, "sandbox-model"); n < 2 || len(applies(s)) != 5 { // those two, the Slack Secret, and the alert source's two (the webhook and the Alertmanager configuration)
		t.Errorf("up.sh applies %d Secrets in all; want five (the model's in each branch, Slack's, and the alert source's two)", len(applies(s)))
	}
	if !strings.Contains(s, `render "${SBX}/modelconfig-${MODEL_PROVIDER}.yaml"`) {
		t.Error("up.sh does not render the provider's own ModelConfig")
	}
}

// walkDestinations is a copy of the walk in kagent 1.0.0-alpha5's agentConfigDestinations
// (go/core/internal/translator/adkconfig/builder.go): every string of the serialised model that parses as a URL with a host
// gives that host as a destination.
func walkDestinations(value any) (hosts []string) {
	switch v := value.(type) {
	case string:
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	case []any:
		for _, item := range v {
			hosts = append(hosts, walkDestinations(item)...)
		}
	case map[string]any:
		for _, item := range v {
			hosts = append(hosts, walkDestinations(item)...)
		}
	}
	return hosts
}

// TestBedrockModelConfigPutsItsEndpointInTheModelJSON: the one header of the Bedrock ModelConfig is how the host reaches kagent's
// destination list. Rendered for any region, its value is the Bedrock endpoint of that region, and kagent's walk over the model JSON
// (type, model, region and headers, as go/api/adk/types.go serialises a Bedrock model) finds exactly the host that kagent's
// credential binding names (credentials.go: "bedrock-runtime." + region + ".amazonaws.com"), which is what the Substrate policy
// compiler requires (substrate/policy.go:32-34) before it accepts the binding. Without the header the walk finds nothing.
func TestBedrockModelConfigPutsItsEndpointInTheModelJSON(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-1", "us-gov-west-1"} {
		t.Run(region, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "bash", "-c", `set -eu
. "$1"
MODEL_NAME="$3" MODEL_REGION="$4" SLACK_CHANNEL_ID=C0RENDER01 render "$2"`, "bash", sandboxDir+"env.sh", sandboxDir+"modelconfig-bedrock.yaml", fakeProfile, region).Output()
			if err != nil {
				t.Fatal(err)
			}
			var mc struct {
				Spec struct {
					Model          string            `json:"model"`
					DefaultHeaders map[string]string `json:"defaultHeaders"`
					Bedrock        struct {
						Region string `json:"region"`
					} `json:"bedrock"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(out, &mc); err != nil {
				t.Fatal(err)
			}
			host := "bedrock-runtime." + region + ".amazonaws.com"
			if len(mc.Spec.DefaultHeaders) != 1 || mc.Spec.DefaultHeaders["X-Agent-Kourier-Egress-Host"] != "https://"+host || mc.Spec.Bedrock.Region != region {
				t.Fatalf("defaultHeaders %v, region %q; want the one header X-Agent-Kourier-Egress-Host: https://%s, and the region", mc.Spec.DefaultHeaders, mc.Spec.Bedrock.Region, host)
			}
			// The model JSON kagent walks.
			model := map[string]any{"type": "bedrock", "model": mc.Spec.Model, "region": mc.Spec.Bedrock.Region, "headers": map[string]any{}}
			if got := walkDestinations(model); len(got) != 0 {
				t.Errorf("the walk finds %v with no headers; want nothing (the gap the header works around), or this test's copy of the walk has drifted", got)
			}
			headers := model["headers"].(map[string]any)
			for k, v := range mc.Spec.DefaultHeaders {
				headers[k] = v
			}
			if got := walkDestinations(model); len(got) != 1 || got[0] != host {
				t.Errorf("the walk finds %v; want exactly %s, the host of the credential binding", got, host)
			}
		})
	}
}
