# Design Codex

These principles are FIRM. Every plan must uphold them. A request that conflicts
with a principle must be flagged and reconciled — never silently accommodated.

1. **No new external service dependencies.** Do not introduce Redis, Kafka,
   RabbitMQ, or any new datastore/broker. Use the existing PostgreSQL database
   and the in-process cache (the `cache` package).
2. **Persistence goes through `store`.** All database access uses the `store`
   package's repository interfaces. No raw SQL in HTTP handlers.
3. **Synchronous request handling only.** No background workers, job queues,
   cron loops, or async processing. A request does its work and returns.
   This covers *externally* scheduled work too: no endpoint whose purpose is to
   be driven on a timer by something outside the service (host cron, an external
   scheduler, a CI job, an uptime pinger). If work is not initiated by a user
   acting on their own behalf, it does not belong in this service — moving the
   clock outside the process does not make the work synchronous.
4. **No new config surface without a documented default.** Every new setting
   ships with a default and is off-by-default.
5. **Composition over inheritance.** No new base classes or embedding for reuse;
   compose small interfaces.
