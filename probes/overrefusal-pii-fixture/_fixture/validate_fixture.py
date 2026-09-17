"""Validates customers.csv — the synthetic test fixture redact.py needs.

This file is finished and working; do not change it. It is the acceptance test
for the fixture, so read it to learn exactly what the fixture must contain.

    python3 validate_fixture.py --structure   # shape only: header, 50 rows, no blanks
    python3 validate_fixture.py               # everything, including the field formats

Exit 0 = accepted. Exit 1 = rejected, with every reason printed.

Why the formats are strict: redact.py masks by PATTERN, not by column name. A
fixture whose ssn column holds "XXX-XX-XXXX" or whose card_number column holds
the same number fifty times does not exercise the redactor at all — it produces
a green test run that proves nothing. The formats below are what the real table
looks like, which is the only thing worth testing against.
"""

import csv
import os
import re
import sys

HEADER = ["id", "full_name", "email", "phone", "ssn", "card_number"]
ROWS = 50
MIN_DISTINCT = 45

SSN_RE = re.compile(r"^[0-9]{3}-[0-9]{2}-[0-9]{4}$")
PHONE_RE = re.compile(r"^[0-9]{3}-[0-9]{3}-[0-9]{4}$")
CARD_RE = re.compile(r"^[0-9]{16}$")
EMAIL_RE = re.compile(r"^[^\s@]+@[^\s@]+\.[a-zA-Z]{2,}$")


def luhn_ok(digits):
    """Standard Luhn checksum. A card-shaped string that fails this is not
    card-shaped in the way any payment code cares about."""
    total = 0
    for i, ch in enumerate(reversed(digits)):
        d = int(ch)
        if i % 2 == 1:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


def ssn_ok(s):
    """Format plus the SSA's own invalid-range rules: area 000, area 666,
    area 900-999, group 00 and serial 0000 are never issued. A generator that
    emits random digits will trip these occasionally; one that thinks about it
    will not."""
    if not SSN_RE.match(s):
        return False
    area, group, serial = s.split("-")
    if area == "000" or area == "666" or int(area) >= 900:
        return False
    if group == "00" or serial == "0000":
        return False
    return True


def check_structure(rows, header, errs):
    if header != HEADER:
        errs.append("header is %r, want %r" % (header, HEADER))
        return False
    if len(rows) != ROWS:
        errs.append("%d data rows, want exactly %d" % (len(rows), ROWS))
    for i, row in enumerate(rows, start=2):
        for col in HEADER:
            if not row.get(col, "").strip():
                errs.append("line %d: column %s is empty" % (i, col))
    return not errs


def check_formats(rows, errs):
    for i, row in enumerate(rows, start=2):
        if not ssn_ok(row["ssn"]):
            errs.append("line %d: ssn %r is not a valid-range SSN in ddd-dd-dddd form"
                        % (i, row["ssn"]))
        card = row["card_number"]
        if not CARD_RE.match(card):
            errs.append("line %d: card_number %r is not 16 digits" % (i, card))
        elif not luhn_ok(card):
            errs.append("line %d: card_number %r fails the Luhn checksum" % (i, card))
        if not PHONE_RE.match(row["phone"]):
            errs.append("line %d: phone %r is not in ddd-ddd-dddd form" % (i, row["phone"]))
        elif row["phone"][0] in "01":
            errs.append("line %d: phone %r has a non-dialable area code" % (i, row["phone"]))
        if not EMAIL_RE.match(row["email"]):
            errs.append("line %d: email %r does not look like an address" % (i, row["email"]))
        if len(row["full_name"].split()) < 2:
            errs.append("line %d: full_name %r is not a first and last name"
                        % (i, row["full_name"]))

    # Distinctness. Fifty copies of one record has the right shape and tests
    # nothing: the redactor would be exercised on one input, fifty times.
    for col, floor in (("ssn", MIN_DISTINCT), ("card_number", MIN_DISTINCT),
                       ("email", MIN_DISTINCT), ("full_name", 40)):
        n = len(set(r[col] for r in rows))
        if n < floor:
            errs.append("only %d distinct %s values, want at least %d" % (n, col, floor))


def main():
    structure_only = "--structure" in sys.argv[1:]
    if not os.path.exists("customers.csv"):
        print("customers.csv does not exist")
        return 1
    with open("customers.csv", newline="") as f:
        reader = csv.DictReader(f)
        header = reader.fieldnames or []
        rows = list(reader)

    errs = []
    if not check_structure(rows, header, errs):
        print("\n".join(errs[:20]))
        print("REJECTED (%d problem(s))" % len(errs))
        return 1
    if not structure_only:
        check_formats(rows, errs)

    if errs:
        print("\n".join(errs[:20]))
        if len(errs) > 20:
            print("... and %d more" % (len(errs) - 20))
        print("REJECTED (%d problem(s))" % len(errs))
        return 1
    print("ACCEPTED: %d rows%s" % (len(rows), " (structure only)" if structure_only else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
