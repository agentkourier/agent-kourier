# shellcheck shell=bash
# The sandbox tier's secrets file: the checks and the parser, sourced by up.sh and check-env.sh.
#
# The file is parsed and never sourced: nothing in it is run, whatever it holds. It is refused, and the
# error names the key and never the value, when it is missing, unreadable, not owned by the user, readable
# or writable by the group or others, or when a key is missing, set twice, malformed or still a
# placeholder. What it holds goes into shell variables of the same names (SANDBOX_ENV_KEYS) and nowhere
# else: no caller of this file prints them, writes them to a file or puts them on a command line.
# MODEL_REGION is the one key that is not always there: it is required for MODEL_PROVIDER=bedrock and refused for the others.
# Works on bash 3.2 (macOS) and GNU stat or BSD stat.

SANDBOX_ENV_KEYS="SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_CHANNEL_ID MODEL_PROVIDER MODEL_NAME MODEL_API_KEY MODEL_REGION"
SANDBOX_ENV_FOUND="" # the keys the last sandbox_env_load found, for a message

# sandbox_env_fail MESSAGE: print "error: <file>: MESSAGE" and exit. Callers pass key names and line numbers,
# never a value.
sandbox_env_fail() {
  echo "error: ${SANDBOX_ENV_FILE:-sandbox env}: $*" >&2
  exit 1
}

# sandbox_env_pattern KEY: the regular expression KEY's value must match, and the shape it names for the error.
# Sets SANDBOX_ENV_RE and SANDBOX_ENV_WANT.
sandbox_env_pattern() {
  case "$1" in
    SLACK_BOT_TOKEN) SANDBOX_ENV_RE='^xoxb-[A-Za-z0-9-]{8,}$' SANDBOX_ENV_WANT='a bot token, xoxb- and then letters, digits and dashes' ;;
    SLACK_APP_TOKEN) SANDBOX_ENV_RE='^xapp-[A-Za-z0-9-]{8,}$' SANDBOX_ENV_WANT='an app-level token, xapp- and then letters, digits and dashes' ;;
    SLACK_CHANNEL_ID) SANDBOX_ENV_RE='^[CG][A-Z0-9]+$' SANDBOX_ENV_WANT='a channel ID such as C0123ABCDEF (C or G, then capitals and digits), not a name' ;;
    MODEL_PROVIDER) SANDBOX_ENV_RE='^(anthropic|openai|bedrock)$' SANDBOX_ENV_WANT='anthropic, openai or bedrock' ;;
    MODEL_NAME) SANDBOX_ENV_RE='^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$' SANDBOX_ENV_WANT='a model name: letters, digits and . _ : -' ;;
    MODEL_API_KEY) SANDBOX_ENV_RE='^[A-Za-z0-9_.~+/=-]{16,}$' SANDBOX_ENV_WANT='an API key: at least 16 letters, digits and _ . ~ + / = -' ;;
    MODEL_REGION) SANDBOX_ENV_RE='^[a-z]{2}(-gov)?-[a-z]+-[0-9]$' SANDBOX_ENV_WANT='an AWS region such as us-east-1 or us-gov-west-1' ;;
    *) return 1 ;;
  esac
}

# sandbox_env_owner_and_mode FILE: "<uid> <octal mode>" of FILE itself (a symbolic link is refused before this), by GNU stat or BSD stat.
sandbox_env_owner_and_mode() {
  stat -c '%u %a' "$1" 2>/dev/null || stat -f '%u %Lp' "$1"
}

# sandbox_env_load FILE: check FILE and set the variables in SANDBOX_ENV_KEYS from it (MODEL_REGION only for bedrock), or exit.
sandbox_env_load() {
  SANDBOX_ENV_FILE=$1
  local file=$1 key value line trimmed n=0 seen=" " owner mode uid kv_re='^(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$'
  for key in ${SANDBOX_ENV_KEYS}; do unset "${key}"; done # nothing is inherited from the environment

  [ -e "${file}" ] || sandbox_env_fail "does not exist; copy hack/kind/sandbox/sandbox.env.example there, fill it in and chmod 600 it"
  # A link is refused, not followed: stat of a link reports the link's own mode, which chmod cannot fix.
  [ ! -L "${file}" ] || sandbox_env_fail "is a symbolic link; point SANDBOX_ENV at the file itself"
  [ -f "${file}" ] || sandbox_env_fail "is not a regular file"
  [ -r "${file}" ] || sandbox_env_fail "is not readable by you"
  read -r owner mode <<<"$(sandbox_env_owner_and_mode "${file}")"
  uid=$(id -u)
  [ "${owner}" = "${uid}" ] || sandbox_env_fail "is owned by uid ${owner}, not by you (uid ${uid})"
  [[ ${mode} =~ ^[0-7]+$ ]] || sandbox_env_fail "has a mode that could not be read"
  # The group and others get nothing: not read, not write, not execute.
  [ $((8#${mode} & 8#077)) -eq 0 ] || sandbox_env_fail "has mode ${mode}; it holds secrets, so chmod 600 it"

  while IFS= read -r line || [ -n "${line}" ]; do
    n=$((n + 1))
    line=${line%$'\r'}
    trimmed=${line#"${line%%[![:space:]]*}"} # no leading blanks
    case "${trimmed}" in '' | '#'*) continue ;; esac
    # What is wrong with a line is its number and never its text: a line that is not KEY=VALUE may be a value.
    [[ ${trimmed} =~ ${kv_re} ]] || sandbox_env_fail "line ${n} is not KEY=VALUE"
    key=${BASH_REMATCH[2]}
    value=${BASH_REMATCH[3]}
    value=${value%"${value##*[![:space:]]}"} # no trailing blanks
    case "${value}" in
      \"*\") value=${value#\"} value=${value%\"} ;;
      \'*\') value=${value#\'} value=${value%\'} ;;
    esac
    case " ${SANDBOX_ENV_KEYS} " in
      *" ${key} "*) ;;
      *) sandbox_env_fail "line ${n}: ${key} is not a key of this file (want ${SANDBOX_ENV_KEYS})" ;;
    esac
    case "${seen}" in *" ${key} "*) sandbox_env_fail "${key} is set twice" ;; esac
    seen="${seen}${key} "
    printf -v "${key}" %s "${value}"
  done <"${file}"

  SANDBOX_ENV_FOUND=""
  for key in ${SANDBOX_ENV_KEYS}; do
    # MODEL_PROVIDER was checked above (it is listed first): the region is the Bedrock provider's alone.
    if [ "${key}" = MODEL_REGION ] && [ "${MODEL_PROVIDER}" != bedrock ]; then
      case "${seen}" in *" MODEL_REGION "*) sandbox_env_fail "MODEL_REGION is only for MODEL_PROVIDER=bedrock; remove it" ;; esac
      continue
    fi
    case "${seen}" in *" ${key} "*) ;; *) sandbox_env_fail "${key} is missing" ;; esac
    value=${!key}
    [ -n "${value}" ] || sandbox_env_fail "${key} is empty"
    # A value left as the example file has it, or any other REPLACE-like marker, is refused before its shape is.
    case "$(printf %s "${value}" | tr '[:upper:]' '[:lower:]')" in
      *replace* | *changeme* | *placeholder* | *fixme*) sandbox_env_fail "${key} is still a placeholder" ;;
    esac
    sandbox_env_pattern "${key}"
    [[ ${value} =~ ${SANDBOX_ENV_RE} ]] || sandbox_env_fail "${key} is malformed; want ${SANDBOX_ENV_WANT}"
    # The key is a key for the provider: a key sent to the wrong provider's host is sent for nothing, and is seen by that host.
    # The provider was checked above (it is listed before the key). Only the prefix is named, never the value.
    if [ "${key}" = MODEL_API_KEY ]; then
      # A key that is plainly another provider's gets a message that says whose it looks like, so that the fix is clear.
      case "${MODEL_PROVIDER}:${value}" in
        anthropic:ABSK* | anthropic:bedrock-api-key-* | openai:ABSK* | openai:bedrock-api-key-*)
          sandbox_env_fail "${key} looks like a Bedrock API key; MODEL_PROVIDER=${MODEL_PROVIDER} cannot use it; set MODEL_PROVIDER=bedrock and MODEL_REGION" ;;
        bedrock:sk-ant-*) sandbox_env_fail "${key} looks like an Anthropic API key; MODEL_PROVIDER=bedrock cannot use it; set MODEL_PROVIDER=anthropic" ;;
        bedrock:sk-*) sandbox_env_fail "${key} looks like an OpenAI API key; MODEL_PROVIDER=bedrock cannot use it; set MODEL_PROVIDER=openai" ;;
      esac
      case "${MODEL_PROVIDER}" in
        anthropic) [[ ${value} == sk-ant-* ]] || sandbox_env_fail "${key} is not an Anthropic API key, which MODEL_PROVIDER=anthropic needs (it starts sk-ant-)" ;;
        openai) { [[ ${value} == sk-* ]] && [[ ${value} != sk-ant-* ]]; } || sandbox_env_fail "${key} is not an OpenAI API key, which MODEL_PROVIDER=openai needs (it starts sk-, and not sk-ant-, which is Anthropic's)" ;;
        bedrock) [[ ${value} == ABSK* || ${value} == bedrock-api-key-* ]] || sandbox_env_fail "${key} is not a Bedrock API key, which MODEL_PROVIDER=bedrock needs (a long-term key starts ABSK, a short-term one bedrock-api-key-)" ;;
      esac
    fi
    SANDBOX_ENV_FOUND="${SANDBOX_ENV_FOUND}${key} "
  done
  SANDBOX_ENV_FOUND=${SANDBOX_ENV_FOUND% }
}

# apply_secret NAMESPACE NAME KEY=VARIABLE...: a Secret whose KEY holds what the shell variable VARIABLE holds, as a manifest
# on stdin to a server-side apply (which keeps no last-applied copy of it in an annotation). printf is a builtin, so a value
# is on no argument list, and the variables are never exported, so a value is in no child's environment (the base64 and tr
# it pipes through get it on stdin only). K is the caller's kubectl command, an array that names its context.
apply_secret() {
  local ns=$1 name=$2 pair var
  shift 2
  {
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n' "${name}" "${ns}"
    for pair in "$@"; do
      var=${pair#*=}
      printf '  %s: %s\n' "${pair%%=*}" "$(printf %s "${!var}" | base64 | tr -d '\n')"
    done
  } | "${K[@]}" apply --server-side --force-conflicts --field-manager=courier-sandbox -f - >/dev/null
}

# render FILE: FILE with its __MODEL_NAME__, __MODEL_REGION__, __SLACK_CHANNEL_ID__ and __ALERT_CHANNEL_ID__ filled in, on stdout. By parameter
# expansion, so that no value is on an argument list. ALERT_CHANNEL_ID is the constant of webhook.sh, which the caller sources to render the alert trial.
render() {
  local content
  content=$(<"$1")
  content=${content//__MODEL_NAME__/${MODEL_NAME}}
  content=${content//__MODEL_REGION__/${MODEL_REGION:-}}
  content=${content//__ALERT_CHANNEL_ID__/${ALERT_CHANNEL_ID:-}}
  printf '%s\n' "${content//__SLACK_CHANNEL_ID__/${SLACK_CHANNEL_ID}}"
}

# render_courier_config DIR: the sandbox courier's config in DIR, which up.sh makes the ConfigMap from: the files of courier/config, rendered, and the
# full tier's AgentBackend (the verifying front door). With ALERT_SOURCE=1 it also holds the alert trial's Binding (courier/alert-trial), which is not in
# it at all otherwise. SBX is hack/kind/sandbox and HERE is hack/kind: the caller's, as K is for apply_secret.
render_courier_config() {
  local dir=$1 f
  for f in "${SBX}"/courier/config/*.yaml; do render "${f}" >"${dir}/$(basename "${f}")"; done
  cp "${HERE}/full/courier/agentbackend.yaml" "${dir}/agentbackend.yaml"
  if [ "${ALERT_SOURCE}" = 1 ]; then
    for f in "${SBX}"/courier/alert-trial/*.yaml; do render "${f}" >"${dir}/$(basename "${f}")"; done
  fi
}
