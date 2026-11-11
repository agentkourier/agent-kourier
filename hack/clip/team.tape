# The team clip: the "whole path, recorded" section of docs/how-to/team-slack-app.md, typed against a real cluster.
# record.sh renders this file (it replaces @VERSION@ with the version in charts/agent-kourier/Chart.yaml, and @CHANNEL@
# and @READY@) and runs it in the recorder image; it is not run by hand. my-values.yaml and team-a.yaml
# are the page's blocks, extracted at each take; the commands below follow the page's section and must be changed with
# it. The Slack tokens are in the shell's environment; the hidden setup writes them to the file the page's command reads,
# so they are only ever named, never printed.

Output out/team-app.gif

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
Type "export PS1='$ ' PS2='  ' && stty -echoctl && . /vhs/setup.sh && cd /work"
Enter
Wait
Type `umask 077 && mkdir -p "$HOME/.config/team-a" && printf 'botToken=%s\nappToken=%s\n' "$SLACK_BOT_TOKEN" "$SLACK_APP_TOKEN" >"$HOME/.config/team-a/slack-secret.env" && chmod 600 "$HOME/.config/team-a/slack-secret.env" && clear`
Enter
Wait
Show

Type "# Agent Kourier @VERSION@, no Slack app of its own: teams bring theirs."
Sleep 200ms
Enter
Wait
Sleep 600ms

# 1. The platform team: a broker that admits team-a and has no ChatConnection of its own.
Type "kubectl create namespace agent-kourier"
Sleep 200ms
Enter
Wait
Sleep 400ms
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

# 2. The team: its app's Secret, from a file, and a grant of it to the broker's ServiceAccount.
Type "# Team team-a: its Slack app's Secret, granted to the broker."
Sleep 200ms
Enter
Wait
Type `kubectl -n team-a create secret generic team-a-slack \`
Enter
Type `--from-env-file="$HOME/.config/team-a/slack-secret.env"`
Sleep 200ms
Enter
Wait
Sleep 400ms
Type `kubectl -n team-a create role agent-kourier-secrets-team-a --verb=list,watch \`
Enter
Type `--resource=secrets --resource-name=team-a-slack`
Sleep 200ms
Enter
Wait
Sleep 400ms
Type `kubectl -n team-a create rolebinding agent-kourier-secrets-team-a --role=agent-kourier-secrets-team-a \`
Enter
Type `--serviceaccount=agent-kourier:agent-kourier`
Sleep 200ms
Enter
Wait
Sleep 750ms
Type "clear"
Enter
Wait

# 3. Its connection, AgentBackend and Binding: the four lines marked "yours".
Type "vim team-a.yaml"
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
Type "http://echo-agent.echo-agent.svc.cluster.local:8080 "
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
Type "kubectl apply -f team-a.yaml"
Sleep 200ms
Enter
Wait
Sleep 750ms

# 4. Ready. The wait for the first probes is cut.
Type "kubectl -n team-a get akbinding -w"
Sleep 200ms
Enter
Wait+Screen /my-binding\s{3,}/
Sleep 1000ms
Hide
Wait+Screen@4m /@READY@/
Show
Sleep 3s
Ctrl+C
Wait
Type "# Mention the team's bot in the channel."
Sleep 2s
