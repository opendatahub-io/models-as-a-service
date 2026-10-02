"""LogQL rewriting for the personal usage dashboard.

The dashboard sends stream selectors `{ key = "value", ... }` with ops
`=`, `!=`, `=~`, optionally `| unwrap tokens_total`, a range `[...]`, and
metric wrappers (`count_over_time`, `sum by`, `or vector(0)`, …). Those
wrappers are left unchanged.

Each selector gets `| user_id="<user>"` inserted immediately after `}`. The
rewritten query is then walked (not matched with a regex): that first
pipeline stage must be exactly `user_id="<user>"` with no following `or`.
"""

_MAX_QUERY_LEN = 32 * 1024
_WS = " \t\r\n"
_MATCH_OPS = ("!=", "=~", "=")  # longer ops first


class QueryError(ValueError):
    """Rejected LogQL (unsupported syntax or missing user scope)."""


def escape_logql_value(value):
    """Escape special characters in LogQL values."""
    return value.replace("\\", "\\\\").replace('"', '\\"')


def inject_user_filter(query, username):
    """
    Inject user filter into a dashboard LogQL query.

    Examples:
        count_over_time({service_name="models-as-a-service", model!="-"} [$__range])
     -> count_over_time({service_name="models-as-a-service", model!="-"} | user_id="alice" [$__range])
    """
    ends = _selector_ends(query)
    extra = f' | user_id="{escape_logql_value(username)}"'
    rewritten = query
    for end in reversed(ends):
        rewritten = rewritten[:end] + extra + rewritten[end:]
    if not _query_requires_user(rewritten, username):
        raise QueryError("rewritten query does not require user_id on every result path")
    return rewritten


def _query_requires_user(query, username):
    """True iff every stream selector is followed by `| user_id="<username>"` as its own stage."""
    try:
        ends = _selector_ends(query)
    except QueryError:
        return False
    if not ends:
        return False
    return all(_first_stage_is_user(query, end, username) for end in ends)


def _selector_ends(query):
    if not query:
        raise QueryError("empty query")
    if len(query) > _MAX_QUERY_LEN:
        raise QueryError("query too long")
    ends = []
    i, n = 0, len(query)
    while i < n:
        ch = query[i]
        if ch == '"':
            _, i = _read_string(query, i)
            continue
        if ch == "`":
            # Loki lexes backtick raw strings. The scanner cannot track them,
            # so a `"` inside one could hide a live selector.
            raise QueryError("raw strings are not supported")
        if ch == "#":
            # Loki discards `#` through end-of-line. A commented `{...}` or `"`
            # must not be treated as a selector or string, or user_id can land
            # in a comment while a live selector is forwarded unscoped.
            raise QueryError("comments are not supported")
        if ch == "$" and i + 1 < n and query[i + 1] == "{":
            j = query.find("}", i + 2)
            if j < 0:
                raise QueryError("unterminated ${...}")
            i = j + 1
            continue
        if ch == "{":
            i = _parse_selector(query, i)
            ends.append(i)
            continue
        i += 1
    if not ends:
        raise QueryError("no stream selector")
    return ends


def _parse_selector(query, i):
    """Parse `{ matcher, matcher }` starting at `{`. Returns index after `}`."""
    s = _Scan(query, i + 1)
    s.skip_ws()
    if s.peek() == "}":
        raise QueryError("empty stream selector")
    s.read_matcher()
    while True:
        s.skip_ws()
        if s.peek() == ",":
            s.i += 1
            s.skip_ws()
            if s.peek() == "}":
                break
            s.read_matcher()
            continue
        break
    s.skip_ws()
    if s.peek() != "}":
        raise QueryError("invalid stream selector")
    return s.i + 1


def _first_stage_is_user(query, selector_end, username):
    s = _Scan(query, selector_end)
    s.skip_ws()
    if s.peek() != "|":
        return False
    s.i += 1
    name, op, value = s.read_matcher()
    if name != "user_id" or op != "=" or value != username:
        return False
    s.skip_ws()
    return s.peek_word() != "or"


class _Scan:
    def __init__(self, text, i=0):
        self.text = text
        self.n = len(text)
        self.i = i

    def peek(self):
        return self.text[self.i] if self.i < self.n else ""

    def skip_ws(self):
        while self.i < self.n and self.text[self.i] in _WS:
            self.i += 1

    def peek_word(self):
        self.skip_ws()
        j = self.i
        while j < self.n and (self.text[j].isalnum() or self.text[j] == "_"):
            j += 1
        return self.text[self.i : j]

    def read_matcher(self):
        self.skip_ws()
        name, self.i = _read_ident(self.text, self.i)
        self.skip_ws()
        op = None
        for candidate in _MATCH_OPS:
            if self.text.startswith(candidate, self.i):
                op = candidate
                self.i += len(candidate)
                break
        if op is None:
            raise QueryError("expected matcher operator")
        self.skip_ws()
        value, self.i = _read_string(self.text, self.i)
        return name, op, value


def _read_string(text, i):
    if i >= len(text) or text[i] != '"':
        raise QueryError("expected string")
    i += 1
    out = []
    while i < len(text):
        ch = text[i]
        if ch == "\\":
            if i + 1 >= len(text):
                raise QueryError("unterminated string")
            out.append(text[i + 1])
            i += 2
            continue
        if ch == '"':
            return "".join(out), i + 1
        out.append(ch)
        i += 1
    raise QueryError("unterminated string")


def _read_ident(text, i):
    n = len(text)
    if i >= n or not (text[i].isalpha() or text[i] == "_"):
        raise QueryError("expected identifier")
    j = i + 1
    while j < n and (text[j].isalnum() or text[j] == "_"):
        j += 1
    return text[i:j], j
