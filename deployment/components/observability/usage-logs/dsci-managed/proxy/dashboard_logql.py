"""Load Perses dashboard LogQL the way the UI sends it (folded YAML and raw lines)."""

from pathlib import Path

from rewriter import QueryError, _read_string, _selector_ends

_DASHBOARDS = Path(__file__).resolve().parent.parent
PERSONAL_DASHBOARD = _DASHBOARDS / "usage-logs-dashboard.yaml"
ADMIN_DASHBOARD = _DASHBOARDS / "usage-logs-admin-dashboard.yaml"


def queries_from_dashboard(path):
    """`expr:` / `query:` strings as Perses sends them (folded YAML and raw lines)."""
    queries = []
    lines = Path(path).read_text().splitlines()
    i = 0
    while i < len(lines):
        raw = lines[i]
        stripped = raw.lstrip()
        indent = len(raw) - len(stripped)
        if stripped.startswith("expr:"):
            val = stripped[len("expr:") :].strip()
            if len(val) >= 2 and val[0] == val[-1] == "'":
                queries.append(val[1:-1])
            i += 1
            continue
        if stripped.startswith("query:") and stripped[len("query:") :].strip() in (
            ">-",
            ">",
            "|-",
            "|",
        ):
            i += 1
            block = []
            while i < len(lines):
                line = lines[i]
                if not line.strip():
                    i += 1
                    continue
                if len(line) - len(line.lstrip()) <= indent:
                    break
                block.append(line.strip())
                i += 1
            queries.append(" ".join(block))
            queries.append("\n".join(block))
            continue
        i += 1
    return queries


def assert_no_top_level_raw_strings(test, query):
    """Fail if a backtick starts a LogQL raw string outside double quotes."""
    i, n = 0, len(query)
    while i < n:
        if query[i] == '"':
            _, i = _read_string(query, i)
            continue
        if query[i] == "$" and i + 1 < n and query[i + 1] == "{":
            j = query.find("}", i + 2)
            if j < 0:
                raise QueryError("unterminated ${...}")
            i = j + 1
            continue
        test.assertNotEqual(
            query[i],
            "`",
            "dashboard query must not use top-level LogQL raw strings",
        )
        i += 1
    test.assertNotIn(
        "`${api_key",
        query,
        "${api_key:...} must not sit inside a LogQL raw string",
    )


def assert_query_is_walkable(test, query):
    """Fail if the rewriter scanner cannot find a stream selector in *query*."""
    ends = _selector_ends(query)
    test.assertGreaterEqual(len(ends), 1, query)
    test.assertGreaterEqual(query.count("{service_name="), 1, query)
