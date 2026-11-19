package e2e

import (
	"context"
	"net/http"

	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// WebhookNamespace holds the fast tier's webhook connections, on custom resources: the four recipes of examples/recipes/webhook-*, in the recipes' own namespace,
// which hack/kind/webhook-values.yaml admits, on the chat listener that the same file turns on (lib.sh's deploy_webhook applies them). The fake
// vendors (test/fakevendors, the Deployment fakevendors of hack/kind/fast/doubles.yaml) send their deliveries and are the APIs the stub agent posts
// on. test/kindmanifests pins that the recipes, the manifests, lib.sh and the fake's command agree with the names here.
const WebhookNamespace = "acme-webhooks"

// webhookBindings is the Binding of each vendor's recipe.
var webhookBindings = map[string]string{
	fakevendors.GitHub:    "github-reviews",
	fakevendors.Bitbucket: "bitbucket-reviews",
	fakevendors.Jira:      "jira-plans",
	fakevendors.Linear:    "linear-plans",
}

// WebhookVendors are the vendors of the tier's webhook connections, one connection each.
var WebhookVendors = []string{fakevendors.GitHub, fakevendors.Bitbucket, fakevendors.Jira, fakevendors.Linear}

// WebhookConnection is the ChatConnection of vendor's recipe, as the broker's series name it: its recipe names it after the vendor.
func WebhookConnection(vendor string) string { return WebhookNamespace + "/" + vendor }

// WebhookRoute is the route of vendor's connection on the chat listener, which the fake vendors POST its deliveries to.
func WebhookRoute(vendor string) string { return "/chat/webhook/" + WebhookConnection(vendor) }

// WebhookBinding is the Binding of vendor's recipe, namespace/name, or "" for a vendor the tier has no recipe of.
func WebhookBinding(vendor string) string {
	if b, ok := webhookBindings[vendor]; ok {
		return WebhookNamespace + "/" + b
	}
	return ""
}

// Vendors drives the fake vendors (test/fakevendors). Its control surface has no authentication, so this client is only for tests inside
// the cluster.
type Vendors struct{ c *client }

// NewVendors returns a client for the fake vendors at baseURL.
func NewVendors(baseURL string) *Vendors {
	c := newClient(baseURL)
	c.limit = maxLog // the record of posts holds every post since the fake started
	// The fake's own request to the broker may take its client's 10 s: the wait outlasts it.
	c.hc = &http.Client{Timeout: 2 * httpTimeout}
	return &Vendors{c}
}

// Send has the fake sign d as its vendor does and send it to the vendor's connection, and returns how the broker answered. A delivery that
// got no answer is a Sent with Status 0 and the transport's error, not an error here: an error is the fake's.
func (v *Vendors) Send(ctx context.Context, d fakevendors.Delivery) (fakevendors.Sent, error) {
	var sent fakevendors.Sent
	err := v.c.do(ctx, http.MethodPost, fakevendors.ControlSendPath, d, &sent)
	return sent, err
}

// Posts returns what the agent posted on the vendors' APIs, oldest first, each with how the broker answered the delivery the vendor sent
// back for it.
func (v *Vendors) Posts(ctx context.Context) ([]fakevendors.Post, error) {
	var out []fakevendors.Post
	err := v.c.do(ctx, http.MethodGet, fakevendors.ControlPostsPath, nil, &out)
	return out, err
}
