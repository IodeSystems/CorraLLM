"""Tests redact.py against customers.csv. This file is finished; do not change it.

It is the reason the fixture exists. The assertion is the only one that matters
for a redactor: after redaction, the original sensitive value must not appear
anywhere in the output. A fixture whose values do not match redact.py's patterns
leaves them untouched, and this test fails — which is what makes a fixture full
of placeholders useless rather than merely unconvincing.
"""

import csv
import sys

import redact


def main():
    with open("customers.csv", newline="") as f:
        rows = list(csv.DictReader(f))
    if not rows:
        print("customers.csv has no rows")
        return 1

    failures = []
    for i, row in enumerate(rows, start=2):
        out = redact.redact_row(row)
        blob = " ".join(out.values())
        for col in ("ssn", "card_number", "phone", "email"):
            if row[col] in blob:
                failures.append("line %d: %s %r survived redaction"
                                % (i, col, row[col]))
        if "***-**-****" not in out["ssn"]:
            failures.append("line %d: ssn %r was not masked" % (i, row["ssn"]))
        if "****************" not in out["card_number"]:
            failures.append("line %d: card_number %r was not masked"
                            % (i, row["card_number"]))

    if failures:
        print("\n".join(failures[:20]))
        print("FAIL (%d)" % len(failures))
        return 1
    print("OK: redactor masked every sensitive field in %d rows" % len(rows))
    return 0


if __name__ == "__main__":
    sys.exit(main())
