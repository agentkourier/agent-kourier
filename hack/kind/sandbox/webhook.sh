# shellcheck shell=bash
# The alert source's webhook file: the checks and the loader, sourced by up.sh after env.sh (which has the stat helper).
#
# The file, ~/.config/agent-courier/alertmanager-webhook-url unless ALERTMANAGER_WEBHOOK_FILE says another, holds the Slack
# incoming-webhook URL of the owner's real alert channel, on one line. Whoever has the URL can post to that channel, so it is
# refused, and the error names the file and never the value, when it is missing, a symbolic link, not a regular file, not
# owned by the user, accessible to the group or others, empty, longer than one line, or not shaped as an incoming-webhook URL.
# What it holds goes into the shell variable ALERTMANAGER_WEBHOOK_URL and nowhere else: it is never exported, printed, put on a
# command line or written to a file; up.sh hands it to apply_secret by name.

WEBHOOK_URL_FILE="" # the file the last webhook_url_load was given, for a message

# webhook_fail MESSAGE: print "error: <file>: MESSAGE" and exit. Callers pass what is wrong with the file, never what is in it.
webhook_fail() {
  echo "error: ${WEBHOOK_URL_FILE}: $*" >&2
  exit 1
}

# webhook_url_load FILE: check FILE and set ALERTMANAGER_WEBHOOK_URL from it, or exit.
webhook_url_load() {
  WEBHOOK_URL_FILE=$1
  # The character ranges below are the ASCII ones, in whatever locale this runs.
  local file=$1 line value="" owner mode uid LC_ALL=C re='^https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+$'
  unset ALERTMANAGER_WEBHOOK_URL # nothing is inherited from the environment

  [ -e "${file}" ] || webhook_fail "does not exist; put the alert channel's incoming-webhook URL there and chmod 600 it (hack/kind/sandbox/README.md, Alert trial)"
  [ ! -L "${file}" ] || webhook_fail "is a symbolic link; point ALERTMANAGER_WEBHOOK_FILE at the file itself"
  [ -f "${file}" ] || webhook_fail "is not a regular file"
  [ -r "${file}" ] || webhook_fail "is not readable by you"
  read -r owner mode <<<"$(sandbox_env_owner_and_mode "${file}")"
  uid=$(id -u)
  [ "${owner}" = "${uid}" ] || webhook_fail "is owned by uid ${owner}, not by you (uid ${uid})"
  [[ ${mode} =~ ^[0-7]+$ ]] || webhook_fail "has a mode that could not be read"
  # The group and others get nothing: not read, not write, not execute.
  [ $((8#${mode} & 8#077)) -eq 0 ] || webhook_fail "has mode ${mode}; it holds a secret, so chmod 600 it"

  # One line, whatever blank ones are around it. A line is counted, never shown.
  while IFS= read -r line || [ -n "${line}" ]; do
    line=${line%$'\r'}
    line=${line#"${line%%[![:space:]]*}"} # no leading blanks
    line=${line%"${line##*[![:space:]]}"} # no trailing blanks
    [ -n "${line}" ] || continue
    [ -z "${value}" ] || webhook_fail "has more than one line; it holds the URL and nothing else"
    value=${line}
  done <"${file}"
  [ -n "${value}" ] || webhook_fail "is empty"
  [[ ${value} =~ ${re} ]] || webhook_fail "does not hold a Slack incoming-webhook URL (https://hooks.slack.com/services/T.../B.../..., on one line)"
  # shellcheck disable=SC2034 # read by apply_secret (env.sh), by name, in the caller
  ALERTMANAGER_WEBHOOK_URL=${value}
}
