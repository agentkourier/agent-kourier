package chart_test

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The AX add-on's own keys (addons.ax): the model, its provider's credentials and the ServiceAccount's annotations. addons_test.go tests
// what it has in common with every add-on.

// axAddOn is the AX add-on of addOns.
func axAddOn() addOn {
	return addOns[slices.IndexFunc(addOns, func(a addOn) bool { return a.name == "ax" })]
}

// axAnthropicOn is the add-on with the least it needs on the Anthropic API, on the minimal install.
var axAnthropicOn = []string{"-f", values("minimal"), "--set",
	"addons.ax.enabled=true,addons.ax.model.provider=anthropic,addons.ax.model.name=claude-haiku-4-5," +
		"addons.ax.anthropicApiKeySecret.name=anthropic,addons.ax.anthropicApiKeySecret.key=apiKey,addons.ax.auth.tokenSecret.name=ax-token"}

// axRefusals are the refusals of the AX add-on's own keys, after its least settings on Bedrock or, for the Anthropic API, after
// axAnthropicOn.
var axRefusals = func() []refusal {
	anthropic := func(name, set, want string) refusal {
		return refusal{name, []string{"--set", set}, want, axAnthropicOn}
	}
	return []refusal{
		{"no region", []string{"--set", "addons.ax.model.region="}, "addons.ax.model.provider bedrock needs addons.ax.model.region", nil},
		{"no model", []string{"--set", "addons.ax.model.name="}, "addons.ax.enabled needs addons.ax.model.name", nil},
		anthropic("no model on the Anthropic API", "addons.ax.model.name=", "addons.ax.enabled needs addons.ax.model.name"),
		{"a provider other than Bedrock or Anthropic", []string{"--set", "addons.ax.model.provider=openai"}, "/addons/ax/model/provider", nil},
		{"an empty provider", []string{"--set", "addons.ax.model.provider="}, "/addons/ax/model/provider", nil},
		anthropic("a region on the Anthropic API", "addons.ax.model.region=us-east-1", "addons.ax.model.region is for addons.ax.model.provider bedrock"),
		anthropic("no Anthropic key Secret", "addons.ax.anthropicApiKeySecret.name=", "addons.ax.model.provider anthropic needs addons.ax.anthropicApiKeySecret.name and addons.ax.anthropicApiKeySecret.key"),
		anthropic("an Anthropic key Secret with no key", "addons.ax.anthropicApiKeySecret.key=", "addons.ax.model.provider anthropic needs addons.ax.anthropicApiKeySecret.name and addons.ax.anthropicApiKeySecret.key"),
		anthropic("a Bedrock key on the Anthropic API", "addons.ax.bedrock.apiKeySecret.name=bedrock,addons.ax.bedrock.apiKeySecret.key=apiKey", "addons.ax.bedrock.apiKeySecret is for addons.ax.model.provider bedrock"),
		anthropic("half a Bedrock key on the Anthropic API", "addons.ax.bedrock.apiKeySecret.key=apiKey", "addons.ax.bedrock.apiKeySecret is for addons.ax.model.provider bedrock"),
		{"an Anthropic key on Bedrock", []string{"--set", "addons.ax.anthropicApiKeySecret.name=anthropic,addons.ax.anthropicApiKeySecret.key=apiKey"}, "addons.ax.anthropicApiKeySecret is for addons.ax.model.provider anthropic", nil},
		{"half an Anthropic key on Bedrock", []string{"--set", "addons.ax.anthropicApiKeySecret.name=anthropic"}, "addons.ax.anthropicApiKeySecret is for addons.ax.model.provider anthropic", nil},
		{"a Bedrock key Secret with no key", []string{"--set", "addons.ax.bedrock.apiKeySecret.name=bedrock"}, "addons.ax.bedrock.apiKeySecret needs both name and key", nil},
		{"a Bedrock key with no Secret", []string{"--set", "addons.ax.bedrock.apiKeySecret.key=apiKey"}, "addons.ax.bedrock.apiKeySecret needs both name and key", nil},
		{"OIDC, which is the AX route's", []string{"--set", "addons.ax.auth.oidc.issuer=https://issuer.example.test"}, "additional properties 'oidc' not allowed", nil},
		{"a runner, which is the AX route's", []string{"--set", "addons.ax.runnerURL=http://127.0.0.1:8081"}, "additional properties 'runnerURL' not allowed", nil},
		{"the CMA add-on's Anthropic rule", []string{"--set", "addons.ax.networkPolicy.anthropic.cidrs={0.0.0.0/0}"}, "additional properties 'anthropic' not allowed", nil},
		{"a rule named for Bedrock", []string{"--set", "addons.ax.networkPolicy.bedrock.cidrs={0.0.0.0/0}"}, "additional properties 'bedrock' not allowed", nil},
		{"an unknown ServiceAccount setting", []string{"--set", "addons.ax.serviceAccount.name=mine"}, "additional properties 'name' not allowed", nil},
	}
}()

// TestAXServiceAccountCarriesTheAWSIdentity: addons.ax.serviceAccount.annotations reach the add-on's ServiceAccount, where IRSA reads its
// role annotation; by default it has none.
func TestAXServiceAccountCarriesTheAWSIdentity(t *testing.T) {
	t.Parallel()
	helmPath(t)
	ax := axAddOn()
	var sa corev1.ServiceAccount
	find(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values(ax.sqlite)), "ServiceAccount", ax.objectName(), &sa)
	if got, want := sa.Annotations["eks.amazonaws.com/role-arn"], "arn:aws:iam::111122223333:role/agent-kourier-ax"; got != want {
		t.Errorf("ServiceAccount annotations %v; want eks.amazonaws.com/role-arn %q", sa.Annotations, want)
	}
	sa = corev1.ServiceAccount{}
	find(t, mustRender(t, "agent-kourier", "agent-kourier", ax.on...), "ServiceAccount", ax.objectName(), &sa)
	if len(sa.Annotations) != 0 {
		t.Errorf("ServiceAccount annotations %v; want none by default", sa.Annotations)
	}
}

// TestAXBedrockKey: with addons.ax.bedrock.apiKeySecret the Bedrock API key reaches the add-on as AWS_BEARER_TOKEN_BEDROCK from its Secret;
// without it the variable is not rendered, and the SDK's default chain applies.
func TestAXBedrockKey(t *testing.T) {
	t.Parallel()
	helmPath(t)
	ax := axAddOn()
	_, env := ax.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", "-f", values(ax.pg)))
	if r := env["AWS_BEARER_TOKEN_BEDROCK"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "bedrock" || r.SecretKeyRef.Key != "apiKey" ||
		env["AWS_BEARER_TOKEN_BEDROCK"].Value != "" {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK %+v; want it from the Secret bedrock's key apiKey alone", env["AWS_BEARER_TOKEN_BEDROCK"])
	}
	_, env = ax.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", ax.on...))
	if _, has := env["AWS_BEARER_TOKEN_BEDROCK"]; has {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK is set without addons.ax.bedrock.apiKeySecret")
	}
}

// TestAXAnthropicKey: with addons.ax.model.provider anthropic the Anthropic API key reaches the add-on as ANTHROPIC_API_KEY from its Secret,
// and neither MODEL_REGION nor the Bedrock key is rendered; with bedrock, ANTHROPIC_API_KEY is not rendered.
func TestAXAnthropicKey(t *testing.T) {
	t.Parallel()
	helmPath(t)
	ax := axAddOn()
	for _, args := range [][]string{axAnthropicOn, {"-f", values("ax-addon-anthropic")}} {
		_, env := ax.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
		if r := env["ANTHROPIC_API_KEY"].ValueFrom; r == nil || r.SecretKeyRef == nil || r.SecretKeyRef.Name != "anthropic" || r.SecretKeyRef.Key != "apiKey" ||
			env["ANTHROPIC_API_KEY"].Value != "" {
			t.Errorf("%v: ANTHROPIC_API_KEY %+v; want it from the Secret anthropic's key apiKey alone", args, env["ANTHROPIC_API_KEY"])
		}
		if env["MODEL_PROVIDER"].Value != "anthropic" || env["MODEL_NAME"].Value != "claude-haiku-4-5" {
			t.Errorf("%v: MODEL_PROVIDER %q, MODEL_NAME %q; want anthropic and claude-haiku-4-5", args, env["MODEL_PROVIDER"].Value, env["MODEL_NAME"].Value)
		}
		for _, name := range []string{"MODEL_REGION", "AWS_BEARER_TOKEN_BEDROCK"} {
			if _, has := env[name]; has {
				t.Errorf("%v: %s is set on the Anthropic API", args, name)
			}
		}
	}
	for _, args := range [][]string{ax.on, {"-f", values(ax.pg)}} {
		_, env := ax.deployment(t, mustRender(t, "agent-kourier", "agent-kourier", args...))
		if _, has := env["ANTHROPIC_API_KEY"]; has {
			t.Errorf("%v: ANTHROPIC_API_KEY is set on Bedrock", args)
		}
	}
}
