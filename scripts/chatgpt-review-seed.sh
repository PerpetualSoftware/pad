#!/usr/bin/env bash
# chatgpt-review-seed.sh: reset the ChatGPT plugin reviewer's demo workspace
# ("Acme Launch") to the known state the plugin's review test cases expect
# (TASK-3321 G8; the test cases are in integrations/chatgpt/plugin.json, the
# seed spec is DOC-3328 section 4).
#
# It talks to Pad only through the pad CLI, as whatever account the CLI is
# already signed in as on the target instance (PAD_URL, or the CLI's
# configured server). It never creates an account and never signs in: set the
# reviewer account up first, then run `pad auth login` as it.
#
# Reset means: every live item in the workspace's non-system collections is
# archived, then the seed items are created again. The test cases name items
# by TITLE, never by ref, so the new refs each run mints are expected, and the
# archived copies are invisible to search and lists. Conventions and
# playbooks are left alone.
#
# Usage:
#   scripts/chatgpt-review-seed.sh --workspace acme-launch --yes
#
# Options:
#   --workspace SLUG   required. Anything other than acme-launch also needs
#                      --allow-other-slug, so a typo cannot wipe a real
#                      workspace.
#   --yes              required: confirms that live items will be archived.
#   --allow-other-slug permit a slug other than acme-launch.
#   --verify-only      change nothing; check that the workspace meets every
#                      test case's preconditions (also run after each reset).
#
# Environment:
#   PAD_URL            target instance (else the CLI's configured server).
#   PAD                pad binary (default: pad on PATH).
#   PAD_REVIEW_SECOND_TOKEN
#                      optional API token of a second workspace member. When
#                      set, the two bug comments are posted as that member
#                      (through the API, since the CLI has one identity);
#                      otherwise the signed-in account posts them.
set -euo pipefail

PAD=${PAD:-pad}
WS=""
YES=0
VERIFY_ONLY=0
ALLOW_OTHER=0
EXPECTED_SLUG="acme-launch"

while [ $# -gt 0 ]; do
  case "$1" in
    --workspace) WS=${2:-}; shift 2 ;;
    --yes) YES=1; shift ;;
    --allow-other-slug) ALLOW_OTHER=1; shift ;;
    --verify-only) VERIFY_ONLY=1; shift ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

die() { echo "chatgpt-review-seed: $*" >&2; exit 1; }

[ -n "$WS" ] || die "--workspace is required"
if [ "$WS" != "$EXPECTED_SLUG" ] && [ "$ALLOW_OTHER" -ne 1 ]; then
  die "refusing to reset workspace '$WS': the review workspace is '$EXPECTED_SLUG' (pass --allow-other-slug to override)"
fi
[ "$YES" -eq 1 ] || [ "$VERIFY_ONLY" -eq 1 ] || die "this archives every live item in '$WS'; re-run with --yes to confirm"
command -v jq >/dev/null || die "jq is required"

p() { "$PAD" --workspace "$WS" "$@"; }

# Writes are paced and retried: a burst of writes trips the API rate limit,
# and a dropped link would leave a test case's precondition silently unmet.
w() {
  local delay=2 out
  for _ in 1 2 3 4 5; do
    if out=$(p "$@" 2>&1); then
      sleep 0.4
      printf '%s' "$out"
      return 0
    fi
    case "$out" in
      *"oo many requests"*) sleep "$delay"; delay=$((delay * 2)) ;;
      *) echo "$out" >&2; return 1 ;;
    esac
  done
  echo "$out" >&2
  return 1
}

# create COLLECTION TITLE [flags...] -> prints the new ref
create() {
  w item create "$@" --format json | jq -er '.ref'
}

# verify checks every precondition the review test cases rely on, through
# the same reads the plugin's tools make. It changes nothing.
fails=0
check() {
  if [ "$2" = "$3" ]; then
    echo "  ok    $1"
  else
    echo "  FAIL  $1: got '$2', want '$3'"
    fails=$((fails + 1))
  fi
}
ref_of() {
  p item list --all --limit 1000 --format json | jq -r --arg t "$1" '[.[] | select(.title==$t)] | if length==1 then .[0].ref else "count=\(length)" end'
}
verify() {
  echo "Verifying test-case preconditions"
  # P1: what_next answers the in-progress onboarding task first.
  check "P1 what_next recommends the onboarding task" \
    "$(p project next --format json | jq -r '.[0].item_title')" "Write onboarding checklist"
  # P2: plausible queries for the login bug each find it.
  for q in "login timing out" "login timeout" "login page slow connection"; do
    check "P2 search '$q' finds the login bug" \
      "$(p item search "$q" --format json | jq -r '[.results[].item.title] | index("Login page times out on slow connections") != null')" "true"
  done
  local bug; bug=$(ref_of "Login page times out on slow connections")
  check "P2 the login bug has two comments" "$(p item comments "$bug" --format json | jq 'length')" "2"
  # P3: the Tasks collection takes priority high.
  check "P3 tasks accept priority high" \
    "$(p collection list --format json | jq -r '.[] | select(.slug=="tasks") | (.schema | if type=="string" then fromjson else . end) | .fields[] | select(.key=="priority") | .options | index("high") != null')" "true"
  # P4: the task exists once, in progress, and done is a valid status.
  local onb; onb=$(ref_of "Write onboarding checklist")
  check "P4 the onboarding task is in progress" \
    "$(p item show "$onb" --format json | jq -r '.fields | if type=="string" then fromjson else . end | .status')" "in-progress"
  check "P4 the welcome doc exists once" "$(ref_of "Welcome to Acme Launch" | grep -c '^DOC-')" "1"
  # P5: the idea exists exactly once.
  check "P5 the obsolete idea exists once" "$(ref_of "Old landing page copy" | grep -c '^IDEA-')" "1"
  # Dashboard and dependencies have something to show.
  local mon; mon=$(ref_of "Set up error monitoring")
  check "the monitoring task is blocked by the vendor task" \
    "$(p item deps "$mon" --format json | jq -r '[.[] | select(.link_type=="blocks" and .target_ref=="'"$mon"'") | .source_title] | join(",")')" "Choose an error monitoring vendor"
  check "two tasks are done" "$(p item list tasks --status done --format json | jq 'length')" "2"
  if [ "$fails" -ne 0 ]; then
    die "$fails precondition(s) failed"
  fi
  echo "All preconditions hold."
}

echo "Target: ${PAD_URL:-the configured server}, workspace '$WS'"
"$PAD" auth whoami >/dev/null 2>&1 || die "the pad CLI is not signed in to the target instance; run 'pad auth login' as the reviewer account first"

if [ "$VERIFY_ONLY" -eq 1 ]; then
  verify
  exit 0
fi

# The workspace must exist on the startup template. Create it if missing.
if ! "$PAD" workspace list --format json | jq -e --arg ws "$WS" 'map(.slug) | index($ws)' >/dev/null; then
  echo "Creating workspace '$WS' from the startup template"
  "$PAD" workspace create "Acme Launch" --slug "$WS" --template startup --format json >/dev/null
fi

collections=$(p collection list --format json)
for c in tasks ideas plans docs; do
  echo "$collections" | jq -e --arg c "$c" 'map(.slug) | index($c)' >/dev/null ||
    die "workspace '$WS' has no '$c' collection; it must be on the startup template"
done

# Test case P4 marks a task done, so the task statuses must be the
# template's. An onboarded workspace can rename them; refuse rather than
# seed a workspace the test cases cannot pass on.
task_statuses=$(echo "$collections" | jq -c '.[] | select(.slug=="tasks") | (.schema | if type=="string" then fromjson else . end) | .fields[] | select(.key=="status") | .options')
[ "$task_statuses" = '["open","in-progress","done","cancelled"]' ] ||
  die "the tasks collection's statuses are $task_statuses, not the startup template's; reset the workspace's Tasks schema first"

if ! echo "$collections" | jq -e 'map(.slug) | index("bugs")' >/dev/null; then
  echo "Creating the Bugs collection"
  w collection create "Bugs" --icon "🐛" --description "Defects to triage and fix" --schema '{
    "fields": [
      {"key": "status", "label": "Status", "type": "select",
       "options": ["open", "fixing", "fixed", "wontfix"],
       "terminal_options": ["fixed", "wontfix"], "abandoned_options": ["wontfix"],
       "default": "open", "required": true},
      {"key": "priority", "label": "Priority", "type": "select",
       "options": ["low", "medium", "high", "critical"], "default": "medium"}
    ]
  }' >/dev/null
fi

# Archive every live item outside the system collections.
echo "Archiving live items"
refs=$(p item list --all --limit 1000 --format json |
  jq -r '.[] | select(.collection_slug != "conventions" and .collection_slug != "playbooks") | .ref')
n=0
for ref in $refs; do
  w item delete "$ref" >/dev/null
  n=$((n + 1))
done
echo "  archived $n"

echo "Seeding"
read -r -d '' login_body <<'EOF' || true
Login is timing out on slow connections. On a throttled connection (Chrome DevTools "Slow 3G"), submitting the login form spins for about 30 seconds and then shows "Something went wrong".

## Steps to reproduce
1. Open the login page with network throttling set to Slow 3G.
2. Enter valid credentials and submit.

## Expected
The user is signed in, however slowly.

## Actual
The request is abandoned after 30 seconds and the form shows a generic error.

## Suspected cause
The login request uses the default 30-second client timeout, and the session lookup after it adds a second round trip.
EOF
login_bug=$(create bugs "Login page times out on slow connections" --status open --priority high --content "$login_body")
create bugs "Avatar upload fails for HEIC images" --status open --priority medium \
  --content "Uploading a HEIC photo from an iPhone as an avatar fails with 'unsupported file'." >/dev/null

onboarding=$(create tasks "Write onboarding checklist" --status in-progress --priority medium \
  --content "A checklist for a new teammate's first week: accounts, repos, the welcome doc, and who to ask.")
pricing=$(create tasks "Ship pricing page v2" --status open --priority high \
  --content "Roll out the redesigned pricing page: new copy, the annual toggle, and the FAQ section.")
monitoring=$(create tasks "Set up error monitoring" --status open --priority medium \
  --content "Report frontend and backend errors to the vendor we choose, with release tags.")
vendor=$(create tasks "Choose an error monitoring vendor" --status open --priority low \
  --content "Compare two or three vendors on price, retention and source-map support.")
for t in "Draft the launch announcement" "Set up the status page"; do
  ref=$(create tasks "$t" --status open --priority medium)
  w item update "$ref" --status done --comment "Finished this week." >/dev/null
done

create ideas "Old landing page copy" --status new \
  --content "Keep the previous landing page copy around in case we want to A/B test it." >/dev/null
create ideas "Dark mode" --status new \
  --content "A dark theme for the app, following the system setting." >/dev/null

launch=$(create plans "Public launch" --status active \
  --content "Everything that has to be true before the public launch.")
create docs "Welcome to Acme Launch" --status published \
  --content "Welcome to the Acme Launch workspace. Start with the onboarding checklist, then pick something from the ready list." >/dev/null

echo "Wiring links"
for child in "$pricing" "$onboarding" "$monitoring"; do
  w item update "$child" --parent "$launch" >/dev/null
done
w item block "$vendor" "$monitoring" >/dev/null

echo "Commenting"
comment_as_second() {
  local ref=$1 msg=$2
  [ -n "${PAD_URL:-}" ] || die "PAD_REVIEW_SECOND_TOKEN needs PAD_URL set to the target instance"
  curl -fsS -X POST "${PAD_URL%/}/api/v1/workspaces/$WS/items/$ref/comments" \
    -H "Authorization: Bearer $PAD_REVIEW_SECOND_TOKEN" -H "Content-Type: application/json" \
    -d "$(jq -n --arg m "$msg" '{body: $m}')" >/dev/null
  sleep 0.4
}
c1="I can reproduce this on a hotel Wi-Fi connection too, so it is not only Slow 3G."
c2="The 30-second figure matches the client timeout in the login form; worth raising it as a first step."
if [ -n "${PAD_REVIEW_SECOND_TOKEN:-}" ]; then
  comment_as_second "$login_bug" "$c1"
  comment_as_second "$login_bug" "$c2"
else
  echo "  PAD_REVIEW_SECOND_TOKEN is not set: the signed-in account posts the bug comments"
  w item comment "$login_bug" "$c1" >/dev/null
  w item comment "$login_bug" "$c2" >/dev/null
fi

echo "Seeded '$WS': login bug $login_bug, onboarding task $onboarding, plan $launch."
verify
