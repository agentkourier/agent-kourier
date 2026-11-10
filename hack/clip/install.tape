# The install clip: the custom resource route of docs/how-to/install-with-helm.md, typed against a real cluster.
# record.sh renders this file (it replaces @VERSION@ with the version in charts/agent-kourier/Chart.yaml, and @CHANNEL@
# and @READY@) and runs it in the recorder image; it is not run by hand. my-values.yaml, platform.yaml and team.yaml are
# the page's blocks, extracted at each take; the commands below follow the page's section and must be changed with it. The Slack tokens are in the
# shell's environment and are only ever named, never printed.

Output out/install.gif

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
Set TypingSpeed 20ms
# 10 frames a second, quick typing and short pauses keep a take near 50 seconds: vhs's GIF render needs about 9 MB a
# frame (measured), so in an 8 GB Docker VM beside the kind node it is killed past roughly 650 frames.
Set Framerate 10
Set WaitPattern /\$\s*$/
Set WaitTimeout 3m
Set Theme { "name": "Agent Kourier", "background": "#0b1020", "foreground": "#e6e9f2", "cursor": "#5b8def", "selection": "#1e3a8a", "black": "#131a2b", "red": "#e5484d", "green": "#3fb27f", "yellow": "#e8b14c", "blue": "#5b8def", "magenta": "#b48cf2", "cyan": "#4cc2d9", "white": "#c9cfdf", "brightBlack": "#6b7590", "brightRed": "#ff6b70", "brightGreen": "#5fd39c", "brightYellow": "#f5c96b", "brightBlue": "#8aaeff", "brightMagenta": "#cdaefc", "brightCyan": "#7dd9ea", "brightWhite": "#ffffff" }

Hide
Type "export PS1='$ ' PS2='  ' && stty -echoctl && . /vhs/setup.sh && cd /work && clear"
Enter
Wait
Show

Type "# Agent Kourier @VERSION@ on custom resources"
Sleep 200ms
Enter
Wait
Sleep 600ms

# 1. The Slack Secret. The values come from the environment.
Type "kubectl create namespace agent-kourier"
Sleep 200ms
Enter
Wait
Sleep 400ms
Type `kubectl -n agent-kourier create secret generic slack \`
Enter
Type `--from-literal=botToken="$SLACK_BOT_TOKEN" \`
Enter
Type `--from-literal=appToken="$SLACK_APP_TOKEN"`
Sleep 200ms
Enter
Wait
Sleep 600ms

# 2. The chart: the CRDs and the broker, told which ChatConnection to serve.
Type "cat my-values.yaml"
Sleep 200ms
Enter
Wait
Sleep 2s
Type `helm install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version @VERSION@ \`
Enter
Type "-n agent-kourier -f my-values.yaml"
Sleep 200ms
Enter
Wait
Sleep 2s
Type "clear"
Enter
Wait

# 3. The platform team's resources, shared with the team's namespace: three lines marked "yours".
Type "# Platform team: the chat app and agent backend, shared with the team."
Sleep 200ms
Enter
Wait
Type "vim platform.yaml"
Sleep 200ms
Enter
Sleep 750ms
Type "/yours"
Sleep 200ms
Enter
Sleep 500ms
Type "0f:w"
Sleep 200ms
Type "ct#"
Sleep 200ms
Type "[echo-agent] "
Escape
Sleep 400ms
Type "j0n0f:w"
Sleep 200ms
Type "ct#"
Type "http://echo-agent.echo-agent.svc.cluster.local:8080 "
Escape
Sleep 300ms
Type "j0n0f:w"
Sleep 200ms
Type "ct#"
Type "[echo-agent] "
Escape
Sleep 300ms
Sleep 450ms
Type ":wq"
Sleep 200ms
Enter
Wait
Sleep 250ms

# 4. The team's Binding, in its own namespace: four lines marked "yours".
Type "# App team: its Binding, in its own namespace."
Sleep 200ms
Enter
Wait
Type "vim team.yaml"
Sleep 200ms
Enter
Sleep 750ms
Type "/yours"
Sleep 200ms
Enter
Sleep 500ms
Type "0f:w"
Sleep 200ms
Type "ct#"
Sleep 200ms
Type "echo-agent "
Escape
Sleep 400ms
Type "j0n0f:w"
Sleep 200ms
Type "ct#"
Type "echo-agent "
Escape
Sleep 300ms
Type "j0n0f:w"
Sleep 200ms
Type "ct#"
Type "echo-agent "
Escape
Sleep 300ms
Type "j0n0f:w"
Sleep 200ms
Type "ct#"
Type "@CHANNEL@ "
Escape
Sleep 300ms
Sleep 450ms
Type ":wq"
Sleep 200ms
Enter
Wait
Sleep 250ms

Type "kubectl apply --dry-run=server -f platform.yaml -f team.yaml"
Sleep 200ms
Enter
Wait
Sleep 1000ms
Type "kubectl apply -f platform.yaml"
Sleep 200ms
Enter
Wait
Sleep 500ms
Type "kubectl apply -f team.yaml"
Sleep 200ms
Enter
Wait
Sleep 750ms

# 5. Ready. The wait for the pod's restart and the first probes is cut.
Type "kubectl get akbinding -A -w"
Sleep 200ms
Enter
Wait+Screen /echo-agent\s+my-binding/
Sleep 1000ms
Hide
Wait+Screen@4m /@READY@/
Show
Sleep 3s
Ctrl+C
Wait
Type "# Mention the bot in the channel."
Sleep 2s
