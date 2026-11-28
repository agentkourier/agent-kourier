# AX E2E tier

`make e2e-ax-up` brings up `kind-agent-kourier-ax-launcher`. `make e2e-ax` runs the AX tests as an in-cluster Job. `make e2e-ax-down` removes the tier. The old ADK cluster, if present, is separate.

The tier uses the AX v0.4.0-rc.2 release installer, its pinned Substrate stack, fake Slack, and two launcher replicas on Postgres. The image registry listens on host loopback and supplies a digest-pinned fixture to Substrate. AX runs it with sandbox debug off. The fixture echoes `AX_GOAL_FILE`, exits with code 7 on `AX_RESULT_FAIL`, and waits on `AX_RESULT_HOLD`. It has no model credentials.

The Gateway replaces the wrapper's Authorization placeholder with the callback Secret before forwarding to a TLS endpoint. The endpoint routes the two exact callback paths to the launcher. The local test CA is installed as an origin trust root in the dedicated egress gateway. The runner and gateway both verify TLS. The broker selects `agent-kourier/slack-main` while a second ChatConnection exists. Its Secret grant excludes the callback Secret.

`E2E_RUN=TestAX_SlackRoundTripAndHistory make e2e-ax` runs one scenario. The first proof used a temporary leader and standby wired through the kit's router. Both the start claim and result returned 204 through the Gateway. The leader matched the runner's metadata Task UID with the control-plane UID. Direct requests with no bearer or the wrong bearer returned 401, and substitution of the actor UID returned 404.

After the Job finishes, run `AX_CLI=/path/to/ax hack/kind/ax/resume-fault.sh` with the same release CLI. The script pauses the dedicated tier's Substrate router until the first AX Resume fails. It checks the queued and running messages in the live Slack thread, then checks that retrying delivery completes the original Task and posts one result. It restores the router replica count, launcher deadline, and broker test watchdog on exit.

0.8.0 ships the launcher tested against AX v0.4.0-rc.2. Repeat the launcher tests against AX v0.4.0 final before the release that moves the add-on to it. A wrapped agent with operator-supplied model credentials has a separate manual smoke described in the [connection guide](../../../docs/how-to/connect-ax.md).
