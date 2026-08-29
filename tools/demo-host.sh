#!/usr/bin/env bash
# Start, stop and inspect the AWS review host.
#
# The instance bills by the second while running and nothing while stopped, only its
# 40GB disk persists, at roughly $3.50/month. So the honest way to use it is: start it
# before a review, stop it after. Everything survives a stop/start, because k3s and all
# its state live on that disk.
#
# There are three independent guards against a surprise bill, in order of how quickly they
# act: the idle auto-stop on the machine itself (90 minutes without a session), the
# budget's stop action at 100% of the cap, and this script.
set -euo pipefail

INSTANCE=${INSTANCE:-i-054dedc804b0ca4e0}
REGION=${REGION:-eu-central-1}
HOURLY=0.2415

state() { aws ec2 describe-instances --region "$REGION" --instance-ids "$INSTANCE" \
  --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null; }
public_ip() { aws ec2 describe-instances --region "$REGION" --instance-ids "$INSTANCE" \
  --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null; }

case "${1:-status}" in
  start)
    echo "starting ${INSTANCE}..."
    aws ec2 start-instances --region "$REGION" --instance-ids "$INSTANCE" >/dev/null
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$INSTANCE"
    IP=$(public_ip)
    echo "running at ${IP}"
    echo
    # The public address changes on every start unless an Elastic IP is attached, and an
    # idle Elastic IP is itself billed, so the address is printed rather than assumed.
    echo "NOTE: the public address changes on each start. Update the security group and"
    echo "      any links you have shared:  make demo-host-allow IP=<address>"
    echo
    echo "billing resumes now: \$${HOURLY}/hour"
    ;;
  stop)
    echo "stopping ${INSTANCE}..."
    aws ec2 stop-instances --region "$REGION" --instance-ids "$INSTANCE" >/dev/null
    aws ec2 wait instance-stopped --region "$REGION" --instance-ids "$INSTANCE"
    echo "stopped. Compute billing has ceased; the 40GB disk continues at ~\$3.50/month."
    ;;
  status)
    S=$(state)
    printf "instance : %s (%s)\n" "$INSTANCE" "$S"
    [ "$S" = "running" ] && printf "address  : %s\n" "$(public_ip)"
    printf "rate     : \$%s/hour while running (\$%.2f/day)\n" "$HOURLY" "$(echo "$HOURLY * 24" | bc -l)"
    echo
    echo "month-to-date against the cap:"
    aws ce get-cost-and-usage --region us-east-1 \
      --time-period "Start=$(date -u +%Y-%m-01),End=$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d tomorrow +%Y-%m-%d)" \
      --granularity MONTHLY --metrics UnblendedCost \
      --filter '{"Tags":{"Key":"CostCenter","Values":["gitops-platform"]}}' \
      --query 'ResultsByTime[0].Total.UnblendedCost.Amount' --output text 2>/dev/null \
      | awk '{printf "  spent: $%.2f of $40 cap (%.0f%%)\n", $1, ($1/40)*100}' \
      || echo "  (cost data lags ~24h; Cost Explorer may not have today's usage yet)"
    ;;
  *)
    echo "usage: demo-host.sh {start|stop|status}"; exit 1 ;;
esac
