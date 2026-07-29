# Architecture Overview

The system is a small read-heavy API fronted by two interchangeable service
tiers (service-a as primary, service-b as failover) that share a worker pool, a
cache tier, and a replicated datastore.

## Request path

Clients resolve the public endpoint and connect over TLS on the production port.
The active front door (service-a in us-west-2) terminates TLS, authenticates the
request, and either serves from cache or fans out to the worker pool. Writes are
routed to the datastore leader, which lives in the primary region.

## Tiers

- **Front door** — service-a (primary, us-west-2) and service-b (failover,
  us-east-1). Both must bind the same production port so a failover is a pure DNS
  swap with no client reconfiguration.
- **Worker pool** — stateless compute; scales horizontally in both regions.
- **Cache** — write-through in us-west-2, standby replica in us-east-1.
- **Datastore** — single leader in the primary region, read replicas in the
  failover region. Writes never go directly to a replica.

## Regions and failover

us-west-2 is primary; us-east-1 is failover. A regional failover promotes an
east replica to leader and repoints the front door — but until that happens, the
east tier serves reads only. Because the two front doors must be swappable, the
single most important invariant is that service-a and service-b agree on the
production port. A mismatch there silently breaks failover.

## Observability

Every tier emits structured JSON logs and Prometheus metrics. The health check
path is /healthz on the production port; three consecutive successes mark a node
healthy for the pool.
