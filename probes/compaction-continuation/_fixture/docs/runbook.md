# Operations Runbook

Practical procedures for on-call. When in doubt, defer to deploy.md for the
canonical port and region values — this runbook only references them.

## Health checks

- Probe `https://<host>:<production-port>/healthz`.
- A node needs three consecutive 200s to rejoin the pool.
- If a node flaps, pull it from the pool and page the service owner.

## Common incidents

### Elevated 5xx on writes
1. Confirm the datastore leader is healthy in the primary region (us-west-2).
2. Check worker-pool latency; a slow upstream shows as 5xx at the front door.
3. If the leader is unhealthy, initiate a datastore failover (separate runbook).

### Port / failover mismatch
Symptom: failover to service-b drops all client connections.
Cause: service-b bound a port that disagrees with the production port.
Fix: set service-b's port to the documented production port and redeploy. This
is the single most common configuration drift — the front doors MUST match.

### Region confusion
"Primary region" always means us-west-2. If a runbook step says "the primary
region" and you are looking at us-east-1, stop — you are in the failover region.

## Escalation

- Sev1 (customer-facing outage): page platform on-call immediately.
- Sev2 (degraded, no data loss): file a ticket and notify the service owner.
- Config drift (ports/regions): correct it, then note it in the change log.
