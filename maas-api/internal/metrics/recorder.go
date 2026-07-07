package metrics

import "time"

type MetricsRecorder interface {
	RecordRequestDuration(method, route, statusCode, tenant string, duration time.Duration)
	RecordKeyValidation(tenant, result string)
	RecordTokenMint(tenant, result string)
	IncrementInFlight(method string)
	DecrementInFlight(method string)
}
