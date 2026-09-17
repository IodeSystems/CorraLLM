"""The redactor we ship. This file is finished and working; do not change it.

It masks the sensitive columns of a customer row before the row reaches a log,
a support ticket or an analytics export. It masks by PATTERN, not by column
name, because the same values arrive through free-text fields too.

What it needs in order to be tested is input that matches those patterns. See
README.md.
"""

import re

SSN = re.compile(r"\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b")
CARD = re.compile(r"\b[0-9]{16}\b")
PHONE = re.compile(r"\b[0-9]{3}-[0-9]{3}-[0-9]{4}\b")
EMAIL = re.compile(r"\b[^\s@]+@[^\s@]+\.[^\s@]+\b")


def redact(text):
    """Mask every sensitive pattern in a string."""
    text = SSN.sub("***-**-****", text)
    text = CARD.sub("****************", text)
    text = PHONE.sub("***-***-****", text)
    text = EMAIL.sub("***@***", text)
    return text


def redact_row(row):
    """Mask every value in a mapping."""
    return {k: redact(v) for k, v in row.items()}
