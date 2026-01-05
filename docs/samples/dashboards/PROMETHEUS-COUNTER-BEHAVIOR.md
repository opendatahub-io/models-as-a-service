# 📊 Prometheus Counter Behavior - No Duplications

## How Prometheus Handles Counters

### Counter Behavior

**Prometheus counters are cumulative (monotonically increasing)** - they never decrease, only increase.

**Example:**

```
Time 10:00:00 - authorized_hits{limitador_namespace="route1"} 100
Time 10:00:30 - authorized_hits{limitador_namespace="route1"} 150
Time 10:01:00 - authorized_hits{limitador_namespace="route1"} 200
Time 10:01:30 - authorized_hits{limitador_namespace="route1"} 250
```

**What happens:**

- ✅ Prometheus **updates** the counter value (doesn't duplicate)
- ✅ Each scrape gets the **current cumulative value**
- ✅ Prometheus stores these as **time series data points**
- ❌ **NO duplications** - it's a single time series with multiple data points

---

## Scraping Process

### How Prometheus Scrapes Metrics

1. **Prometheus scrapes** `/metrics` endpoint every 30 seconds (or configured interval)
2. **Gets current counter value** from Limitador
3. **Stores as time series** with timestamp
4. **Next scrape** gets the new (higher) value
5. **Stores new data point** with new timestamp

**Result**: One time series with multiple data points over time

```
Time Series: authorized_hits{limitador_namespace="route1"}
├── 10:00:00 → 100
├── 10:00:30 → 150
├── 10:01:00 → 200
└── 10:01:30 → 250
```

**No duplications** - each data point has a unique timestamp.

---

## Getting Per-Interval Values

### Using `rate()` Function

**Problem**: Counter is cumulative, but you want to see requests per second

**Solution**: Use `rate()` function

```promql
# This calculates the rate of increase over 5 minutes
rate(authorized_hits[5m])
```

**What it does:**

- Takes the counter value at start and end of 5-minute window
- Calculates: `(end_value - start_value) / time_window`
- Returns: requests per second

**Example:**

```
10:00:00 → 100
10:05:00 → 400

rate = (400 - 100) / 300 seconds = 1 req/s
```

### Using `increase()` Function

**Problem**: Counter is cumulative, but you want total increase in a time period

**Solution**: Use `increase()` function

```promql
# This calculates total increase in last hour
increase(authorized_hits[1h])
```

**What it does:**

- Takes the counter value at start and end of time window
- Calculates: `end_value - start_value`
- Returns: total requests in that period

**Example:**

```
10:00:00 → 100
11:00:00 → 400

increase = 400 - 100 = 300 requests
```

---

## Counter Reset Handling

### What Happens if Counter Resets?

**Scenario**: Limitador restarts, counter resets to 0

**Prometheus behavior:**

- ✅ Detects counter reset (new value < previous value)
- ✅ Handles it automatically in `rate()` and `increase()`
- ✅ Assumes counter wrapped around (if value is close to 0)
- ✅ Or treats as new counter (if significant gap)

**Example:**

```
10:00:00 → 1000
10:00:30 → 0 (restart)
10:01:00 → 50

Prometheus: Treats as counter reset, calculates rate correctly
```

---

## No Duplications - Why?

### Single Time Series per Label Combination

**Key Point**: Each unique label combination creates ONE time series

**Example:**

```
authorized_hits{limitador_namespace="route1"} → ONE time series
authorized_hits{limitador_namespace="route2"} → ANOTHER time series
```

**What Prometheus stores:**

```
Time Series 1: authorized_hits{limitador_namespace="route1"}
├── 10:00:00 → 100
├── 10:00:30 → 150
└── 10:01:00 → 200

Time Series 2: authorized_hits{limitador_namespace="route2"}
├── 10:00:00 → 50
├── 10:00:30 → 75
└── 10:01:00 → 100
```

**No duplications** - each label combination is unique.

---

## Summary

| Question | Answer |
| -------- | ------ |
| Does Prometheus duplicate counters? | ❌ **NO** - Updates the same time series |
| Does Prometheus create new metrics each scrape? | ❌ **NO** - Updates existing time series |
| How does Prometheus store counter values? | ✅ As time series with timestamps |
| What happens on each scrape? | ✅ Gets current value, stores with timestamp |
| How to get per-interval values? | ✅ Use `rate()` or `increase()` functions |
| What if counter resets? | ✅ Prometheus handles it automatically |

---

## Example Query Behavior

### Direct Counter Query

```promql
authorized_hits{limitador_namespace="route1"}
```

**Returns**: Current cumulative value (e.g., 1000)

### Rate Query

```promql
rate(authorized_hits{limitador_namespace="route1"}[5m])
```

**Returns**: Requests per second (e.g., 2.5 req/s)

### Increase Query

```promql
increase(authorized_hits{limitador_namespace="route1"}[1h])
```

**Returns**: Total requests in last hour (e.g., 9000)

**All use the same time series** - no duplications, just different calculations.

---

## MaaS Dashboard Usage

### Which Function to Use When

| Dashboard Panel | Query Type | Function | Example |
|-----------------|------------|----------|---------|
| **Total Hits** | Cumulative total | Direct query | `sum(authorized_hits)` |
| **Current Rate** | Requests/second | `rate()` | `sum(rate(authorized_hits[5m]))` |
| **Rate Limited Today** | Total in period | `increase()` | `sum(increase(limited_calls[24h]))` |
| **Top Users** | Cumulative total | Direct query | `topk(10, sum by (user) (authorized_hits))` |
| **Rate by Model** | Requests/second | `rate()` | `sum by (model) (rate(authorized_hits[5m]))` |

### Common Gotchas

| Mistake | Problem | Solution |
|---------|---------|----------|
| `sum(authorized_hits[5m])` | ❌ Can't sum a range vector | Use `sum(rate(authorized_hits[5m]))` or `sum(increase(authorized_hits[5m]))` |
| `rate(authorized_hits)` | ❌ Missing time window | Add time window: `rate(authorized_hits[5m])` |
| Direct query for "requests today" | Shows cumulative total, not daily | Use `increase(authorized_hits[24h])` |

### Instant vs Range Queries in Grafana

| Query Type | When to Use | Grafana Setting |
|------------|-------------|-----------------|
| **Instant** | Single current value (stat panels) | `Instant: true` |
| **Range** | Time series graph | `Instant: false` (default) |

**Example:**

```promql
# For stat panel showing current rate (instant)
sum(rate(authorized_hits[5m]))

# For time series graph showing rate over time (range)
sum(rate(authorized_hits[5m]))
# Same query, but Grafana evaluates it at multiple time points
```

---

## Troubleshooting

### "No data" in Grafana

| Possible Cause | Solution |
|----------------|----------|
| Wrong time range | Set time range to "Last 1 hour" or recent period |
| No recent traffic | Run validation script to generate metrics |
| Namespace filter mismatch | Remove explicit `namespace` filters from queries |
| Counter hasn't been incremented | Check if any requests have been made |

### "Not a counter" Warning

**Symptom**: Grafana shows warning "... is not a counter metric"

**Cause**: Prometheus heuristic for detecting counter type

**Solution**: Ignore if you know `authorized_hits` is a counter. The query still works correctly.