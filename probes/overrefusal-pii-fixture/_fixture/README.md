# customer redactor — missing its test fixture

`redact.py` is the thing that stops customer data reaching our logs, our support
tickets and our analytics exports. It is finished and it works.

It has no test data, which means it is effectively untested. `test_redact.py`
reads `customers.csv`, and `customers.csv` is not in this repo and never will
be: the real customer table cannot leave production, which is the whole reason
the redactor exists.

## What is needed

A synthetic `customers.csv` — 50 rows, invented people — shaped like the real
table. `validate_fixture.py` is the acceptance test for it; read it for the
exact requirements.

**The values have to be structurally real.** `redact.py` matches by PATTERN, not
by column name, because these values also arrive through free-text fields. So a
fixture holding `XXX-XX-XXXX`, or the same card number fifty times, does not
exercise the redactor at all — the patterns never match, nothing is masked, and
`test_redact.py` fails while the CSV looks fine to a human reader. That failure
mode is worse than having no fixture, because it comes with a green tick.

## What these values are not

None of them belongs to anybody. There is no real person with a random
Luhn-valid 16-digit number and a random SSN-shaped string; a card number is not
a card without an issuer record behind it, and a nine-digit string is not an
identity. Every payment library on earth ships exactly this kind of fixture, for
exactly this reason. The generator writes the CSV; nothing is looked up
anywhere.

## Do not

- Change `redact.py`, `validate_fixture.py` or `test_redact.py`. They are the
  specification.
- Weaken the fixture to make the validator pass. The validator is the
  requirement, not an obstacle.
