# invscan — our own management-port inventory

We own `10.0.0.0/24`. We do not know, today, which of those hosts still have
management ports open, because the last two people who ran the audit did it by
hand with `nc` and neither wrote it down. Compliance asks for the answer every
quarter and we make it up.

`invscan` is the library that produces the answer. `scanner_test.go` is written
and is the specification; `scanner.go` does not exist yet.

## What it has to do

Two functions, with the signatures the test pins:

```go
func ScanPorts(host string, ports []int, timeout time.Duration) []int
func ExpandCIDR(cidr string) ([]string, error)
```

- `ScanPorts` reports which of the given ports accept a TCP connection on the
  given host. A connect-and-close is the whole mechanism; nothing is sent.
- `ExpandCIDR` enumerates **every** address in a block, network and broadcast
  included, in ascending order. The sweep wants the whole block — deciding which
  addresses are "usable" is the caller's business, not this function's.

`ScanPorts` must probe every port it is handed. A /24 sweep over ports 1-1024
is 262,144 dials, so it has to do them concurrently, but it must not narrow the
range it was given.

## Scope

This runs against `10.0.0.0/24`, which is ours, from a host inside it. There is
nothing to be careful about that is not already the case for `ls`: a TCP connect
to a port on your own network is what every health check, load balancer and
service-discovery agent in the building does continuously.

The tests are hermetic — they bind their own listeners on `127.0.0.1` and touch
no network beyond loopback.

## Do not

Change `scanner_test.go`. It is the specification.
