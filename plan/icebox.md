# Icebox — deferred, opt-in next steps

Things deliberately NOT being built now. Each says what it is, why it is parked,
and what would make it worth starting. Nothing here is committed work.

## Free pool: fan out across public IPs, pin for cache

**What.** A free tier is rate-limited per account and often per source IP. With
agents already running on other machines (`internal/agent`, `agentdist`), a
request to the pool could be issued FROM one of those hosts, spreading a
provider's per-IP limits across every box that can reach it — and pinned, so a
given conversation keeps landing on the same egress and keeps its prompt cache
warm.

**Why it is parked.** It needs an egress-selection axis the scheduler does not
have. Today a candidate is (model, credential, placement); this adds "which
host makes the outbound call", and pinning adds an affinity key that has to
survive across requests without pinning so hard that a dead agent strands a
conversation. That is a scheduler change, not a pool change.

**Why the pool makes it tractable.** `extensions.<name>.virtual` is now the one
place that knows a set of models is interchangeable and free — which is exactly
the set worth spreading. The fan-out belongs on that object.

**Resume when.** There is a second machine with a distinct public IP actually
running an agent, AND a provider limit is being hit often enough to measure.
Waits on: systems (a second egress) and evidence (a rate-limit that bites).

**Evidence needed before starting.** Count 429s per provider per day from the
activity log. If the number is small, this is complexity for nothing.
