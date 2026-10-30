# Names vulture reports as unused but that are used outside the parsed code; not shipped
# (kustomization.yaml lists the proxy files explicitly). Parsed by vulture, never imported.
# ruff: noqa

# BaseHTTPRequestHandler dispatches do_<METHOD>, calls log_message and reads protocol_version.
_.do_GET
_.log_message
protocol_version
format
args

# Read by the logging format string as %(request_id)s.
_.request_id
