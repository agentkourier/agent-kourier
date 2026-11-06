# The install clip: the custom resource route of docs/how-to/install-with-helm.md, typed against a real cluster.
# record.sh renders this file (it replaces @VERSION@ with the version in charts/agent-kourier/Chart.yaml, and @CHANNEL@
# and @READY@) and runs it in the recorder image; it is not run by hand. my-values.yaml and my-resources.yaml are the
# page's blocks, extracted at each take; the commands below follow the page's section and must be changed with it. The Slack tokens are in the
# shell's environment and are only ever named, never printed.

Output out/install.gif
Output out/install.mp4

Require kubectl
Require helm
Require vim

Set Shell bash
Set FontFamily "JetBrains Mono"
Set FontSize 20
Set LineHeight 1.25
Set Width 1320
Set Height 760
Set Padding 28
Set WindowBar Colorful
Set BorderRadius 10
Set Margin 0
Set CursorBlink false
Set TypingSpeed 45ms
Set Framerate 24
Set WaitPattern /\$\s*$/
Set WaitTimeout 3m
Set Theme { "name": "Agent Kourier", "background": "#0b1020", "foreground": "#e6e9f2", "cursor": "#5b8def", "selection": "#1e3a8a", "black": "#131a2b", "red": "#e5484d", "green": "#3fb27f", "yellow": "#e8b14c", "blue": "#5b8def", "magenta": "#b48cf2", "cyan": "#4cc2d9", "white": "#c9cfdf", "brightBlack": "#6b7590", "brightRed": "#ff6b70", "brightGreen": "#5fd39c", "brightYellow": "#f5c96b", "brightBlue": "#8aaeff", "brightMagenta": "#cdaefc", "brightCyan": "#7dd9ea", "brightWhite": "#ffffff" }

Hide
Type "export PS1='$ ' PS2='  ' && stty -echoctl && cd /work && clear"
Enter
Wait
Show

Type "# Agent Kourier @VERSION@ on custom resources"
Sleep 300ms
Enter
Wait
Sleep 1.2s

# 1. The Slack Secret. The values come from the environment.
Type "kubectl create namespace agent-kourier"
Sleep 300ms
Enter
Wait
Sleep 800ms
Type `kubectl -n agent-kourier create secret generic agent-kourier-slack \`
Enter
Type `--from-literal=botToken="$SLACK_BOT_TOKEN" \`
Enter
Type `--from-literal=appToken="$SLACK_APP_TOKEN"`
Sleep 300ms
Enter
Wait
Sleep 1.2s

# 2. The chart: the CRDs and the broker, told which ChatConnection to serve.
Type "cat my-values.yaml"
Sleep 300ms
Enter
Wait
Sleep 2.5s
Type `helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version @VERSION@ \`
Enter
Type "-n agent-kourier -f my-values.yaml"
Sleep 300ms
Enter
Wait
Sleep 2.5s
Type "clear"
Enter
Wait

# 3. The resources: the four lines marked "yours".
Type "vim my-resources.yaml"
Sleep 300ms
Enter
Sleep 1.5s
Type "/yours"
Sleep 400ms
Enter
Sleep 1s
Type "0f:w"
Sleep 300ms
Type "ct#"
Sleep 300ms
Type "http://echo-agent.echo-agent.svc.cluster.local:8080 "
Escape
Sleep 800ms
Type "j0n0f:w"
Sleep 300ms
Type "ct#"
Type "echo-agent "
Escape
Sleep 600ms
Type "j0n0f:w"
Sleep 300ms
Type "ct#"
Type "echo-agent "
Escape
Sleep 600ms
Type "j0n0f:w"
Sleep 300ms
Type "ct#"
Type "@CHANNEL@ "
Escape
Sleep 1.5s
Type ":wq"
Sleep 300ms
Enter
Wait
Sleep 500ms

Type "kubectl apply --dry-run=server -f my-resources.yaml"
Sleep 300ms
Enter
Wait
Sleep 2s
Type "kubectl apply -f my-resources.yaml"
Sleep 300ms
Enter
Wait
Sleep 1.5s

# 4. Ready. The wait for the pod's restart and the first probes is cut.
Type "kubectl get akbinding -A -w"
Sleep 300ms
Enter
Wait+Screen /agent-kourier\s+my-binding/
Sleep 2s
Hide
Wait+Screen@4m /@READY@/
Show
Sleep 3.5s
Ctrl+C
Wait
Type "# Mention the bot in the channel."
Sleep 2.5s
