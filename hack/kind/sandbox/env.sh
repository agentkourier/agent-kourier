# shellcheck shell=bash
# The sandbox tier's secrets file: the checks and the parser, sourced by up.sh and check-env.sh.
#
# The file is parsed and never sourced: nothing in it is run, whatever it holds. It is refused, and the
# error names the key and never the value, when it is missing, unreadable, not owned by the user, readable
# or writable by the group or others, or when a key is missing, set twice, malformed or still a
# placeholder. What it holds goes into shell variables of the same names (SANDBOX_ENV_KEYS) and nowhere
# else: no caller of this file prints them, writes them to a file or puts them on a command line.
# Works on bash 3.2 (macOS) and GNU stat or BSD stat.

SANDBOX_ENV_KEYS="SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_CHANNEL_ID MODEL_PROVIDER MODEL_NAME MODEL_API_KEY"

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
    MODEL_PROVIDER) SANDBOX_ENV_RE='^(anthropic|openai)$' SANDBOX_ENV_WANT='anthropic or openai' ;;
    MODEL_NAME) SANDBOX_ENV_RE='^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$' SANDBOX_ENV_WANT='a model name: letters, digits and . _ : -' ;;
    MODEL_API_KEY) SANDBOX_ENV_RE='^[A-Za-z0-9_.~+/=-]{16,}$' SANDBOX_ENV_WANT='an API key: at least 16 letters, digits and _ . ~ + / = -' ;;
    *) return 1 ;;
  esac
}

# sandbox_env_owner_and_mode FILE: "<uid> <octal mode>" of the file the path resolves to, by GNU stat or BSD stat.
sandbox_env_owner_and_mode() {
  stat -c '%u %a' "$1" 2>/dev/null || stat -f '%u %Lp' "$1"
}

# sandbox_env_load FILE: check FILE and set the six variables in SANDBOX_ENV_KEYS from it, or exit.
sandbox_env_load() {
  SANDBOX_ENV_FILE=$1
  local file=$1 key value line trimmed n=0 seen=" " owner mode uid kv_re='^(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$'
  for key in ${SANDBOX_ENV_KEYS}; do unset "${key}"; done # nothing is inherited from the environment

  [ -e "${file}" ] || sandbox_env_fail "does not exist; copy hack/kind/sandbox/sandbox.env.example there, fill it in and chmod 600 it"
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

  for key in ${SANDBOX_ENV_KEYS}; do
    case "${seen}" in *" ${key} "*) ;; *) sandbox_env_fail "${key} is missing" ;; esac
    value=${!key}
    [ -n "${value}" ] || sandbox_env_fail "${key} is empty"
    # A value left as the example file has it, or any other REPLACE-like marker, is refused before its shape is.
    case "$(printf %s "${value}" | tr '[:upper:]' '[:lower:]')" in
      *replace* | *changeme* | *placeholder* | *fixme*) sandbox_env_fail "${key} is still a placeholder" ;;
    esac
    sandbox_env_pattern "${key}"
    [[ ${value} =~ ${SANDBOX_ENV_RE} ]] || sandbox_env_fail "${key} is malformed; want ${SANDBOX_ENV_WANT}"
  done
}
