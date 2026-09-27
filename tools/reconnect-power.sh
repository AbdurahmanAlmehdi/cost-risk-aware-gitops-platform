#!/usr/bin/env bash
# Point the power switch at a different review host.
#
#   tools/reconnect-power.sh i-0123456789abcdef0
#
# The instance id appears in FOUR places that must agree, and three of them are
# outside this repository. Miss one and the failure is quiet and confusing: the
# page still loads, the button still looks live, and AWS answers
# "You are not authorized to perform this operation" with no hint that the id it
# was asked about is not the id the policy allows.
#
#   1. The IAM inline policy         the only thing that actually gates the API call
#   2. The Worker's INSTANCE_ID var  what the button asks about
#   3. The budget's stop action      the $40 cap's emergency brake
#   4. tools/demo-host.sh            the same switch from a terminal
#
# This exists because the host is disposable by design: it is restored from a
# snapshot whenever it is needed, and every restore produces a new id.
#
# Idempotent. Run it again after any rebuild.
set -euo pipefail

INSTANCE="${1:-}"
REGION="${REGION:-eu-central-1}"
ACCOUNT="${ACCOUNT:-240571106679}"
IAM_USER="${IAM_USER:-gitops-platform-power}"
POLICY_NAME="power-cycle-review-host"
BUDGET="${BUDGET:-gitops-platform-demo-cap}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ ! "$INSTANCE" =~ ^i-[0-9a-f]{8,17}$ ]]; then
  echo "usage: tools/reconnect-power.sh <instance-id>" >&2
  exit 1
fi

ARN="arn:aws:ec2:${REGION}:${ACCOUNT}:instance/${INSTANCE}"

echo "==> Checking the instance exists"
STATE="$(aws ec2 describe-instances --region "$REGION" --instance-ids "$INSTANCE" \
  --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || true)"
if [ -z "$STATE" ] || [ "$STATE" = "None" ] || [ "$STATE" = "terminated" ]; then
  echo "FATAL: ${INSTANCE} does not exist or is terminated (state: ${STATE:-none})" >&2
  exit 1
fi
echo "    ${INSTANCE} is ${STATE}"

# --- 1. IAM ---------------------------------------------------------------
# Rewritten from the known-good shape rather than patched in place: an inline
# policy is small, and generating it is one less thing that can drift.
echo "==> IAM: ${IAM_USER}/${POLICY_NAME}"
aws iam put-user-policy --user-name "$IAM_USER" --policy-name "$POLICY_NAME" \
  --policy-document "$(cat <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["ec2:StartInstances", "ec2:StopInstances"],
      "Resource": "${ARN}"
    },
    {
      "Effect": "Allow",
      "Action": ["ec2:DescribeInstances"],
      "Resource": "*"
    }
  ]
}
JSON
)"

# Prove it, rather than assume it. simulate-principal-policy asks IAM the same
# question the Worker's signed request will ask, without starting anything.
echo "==> Verifying the policy actually allows it"
for action in ec2:StartInstances ec2:StopInstances; do
  DECISION="$(aws iam simulate-principal-policy \
    --policy-source-arn "arn:aws:iam::${ACCOUNT}:user/${IAM_USER}" \
    --action-names "$action" --resource-arns "$ARN" \
    --query 'EvaluationResults[0].EvalDecision' --output text)"
  printf '    %-22s %s\n' "$action" "$DECISION"
  [ "$DECISION" = "allowed" ] || { echo "FATAL: IAM would deny ${action}" >&2; exit 1; }
done

# --- 2. Budget action -----------------------------------------------------
# The cap's stop action. Left pointing at a dead instance it fails silently at
# exactly the moment it is supposed to save money.
echo "==> Budget action on ${BUDGET}"
ACTION_ID="$(aws budgets describe-budget-actions-for-budget \
  --account-id "$ACCOUNT" --budget-name "$BUDGET" \
  --query 'Actions[0].ActionId' --output text 2>/dev/null || true)"
if [ -n "$ACTION_ID" ] && [ "$ACTION_ID" != "None" ]; then
  aws budgets update-budget-action --account-id "$ACCOUNT" --budget-name "$BUDGET" \
    --action-id "$ACTION_ID" \
    --definition "{\"SsmActionDefinition\":{\"ActionSubType\":\"STOP_EC2_INSTANCES\",\"Region\":\"${REGION}\",\"InstanceIds\":[\"${INSTANCE}\"]}}" \
    >/dev/null
  echo "    updated ${ACTION_ID}"
else
  echo "    none found — skipping (the idle auto-stop and the Worker still apply)"
fi

# --- 3 & 4. The repository ------------------------------------------------
echo "==> Repository references"
OLD_IDS="$(grep -rhoE 'i-[0-9a-f]{8,17}' \
  "$REPO_ROOT/edge-control/wrangler.toml" \
  "$REPO_ROOT/edge-control/README.md" \
  "$REPO_ROOT/tools/demo-host.sh" 2>/dev/null | sort -u | grep -v "^${INSTANCE}$" || true)"
if [ -z "$OLD_IDS" ]; then
  echo "    already current"
else
  for old in $OLD_IDS; do
    # macOS and GNU sed disagree about -i, so write through a temp file instead.
    for f in edge-control/wrangler.toml edge-control/README.md tools/demo-host.sh; do
      [ -f "$REPO_ROOT/$f" ] || continue
      if grep -q "$old" "$REPO_ROOT/$f"; then
        sed "s/${old}/${INSTANCE}/g" "$REPO_ROOT/$f" > "$REPO_ROOT/$f.tmp"
        mv "$REPO_ROOT/$f.tmp" "$REPO_ROOT/$f"
        echo "    ${f}: ${old} -> ${INSTANCE}"
      fi
    done
  done
fi

# --- 5. The Worker --------------------------------------------------------
# INSTANCE_ID is a [vars] entry, so it only reaches Cloudflare on a deploy.
# `wrangler deploy` does not touch secrets, so the AWS key pair survives.
echo "==> Deploying the Worker"
( cd "$REPO_ROOT/edge-control" && npx --yes wrangler deploy ) 2>&1 | sed 's/^/    /'

cat <<DONE

Done. The switch now controls ${INSTANCE}.

  Press it:  https://power.abdurahman.ly
  Or:        make demo-host-status

Commit the three changed files — the next person to read them should not find a
dead instance id.
DONE
