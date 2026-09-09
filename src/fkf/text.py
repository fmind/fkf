"""Shared Unicode-stable lexical normalization; no corpus retained globally."""

import re

_TOKENS = re.compile(r"[\w\-.@/:#%]+")
_NON_ASCII = re.compile(r"[^\x00-\x7f]")


def lower(value: str) -> str:
    """Lower individual code points without expanding or contextual mappings."""
    if value.isascii():
        return value.lower()
    # Whole-string lower changes final sigma and expands dotted I. Translate
    # individual non-ASCII characters to preserve the durable ranking contract.
    mapping = {ord(character): character.lower() for character in set(_NON_ASCII.findall(value))}
    mapping = {key: mapped for key, mapped in mapping.items() if len(mapped) == 1}
    mapping.update({code: chr(code + 32) for code in range(65, 91)})
    return value.translate(mapping)


def terms(value: str) -> tuple[str, ...]:
    """Tokenize letters/numbers plus FKF's identifier punctuation."""
    return tuple(_TOKENS.findall(lower(value)))
