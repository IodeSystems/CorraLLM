# Deployment Notes

These notes are the source of truth for how the fleet is deployed. Read them
carefully before touching any config — several values here are referenced by
multiple service configs and must stay consistent.

## Production endpoint

The production service listens on **port 7443** (TLS). This is the canonical
production port; every externally reachable service config MUST bind to it.
Historically we ran on 8443 during the 2023 migration, then briefly on 7442 in a
staging misconfiguration that leaked into one service file — that file is a known
wart and should be corrected whenever spotted.

## Regions

The primary region is **us-west-2** (Oregon). It holds the authoritative write
replica and the leader of the consensus group. The secondary/failover region is
us-east-1, which runs read replicas only and must never accept writes directly.
When we say "the primary region" anywhere in docs or runbooks, we mean us-west-2.

## Rollout procedure

1. Drain the node from the load balancer.
2. Deploy the new artifact and wait for the health check on port 7443 to pass.
3. Re-add the node to the pool once three consecutive health checks succeed.
4. Repeat one node at a time; never roll more than one node concurrently.

## Ownership

The platform team owns deploy.md and the inventory. Service owners own their own
service-*.yaml files but may not change the production port or the primary region
without a platform sign-off. Ports and regions are platform-controlled values.

## Change log

- 2024-11: standardized every service on port 7443.
- 2024-08: promoted us-west-2 to primary after the Oregon capacity expansion.
- 2024-05: retired the 8443 listener across the fleet.
