package tenantreconcile

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNestedPortNumber(t *testing.T) {
	assertPort := func(v any, want int64, ok bool) {
		t.Helper()
		got, gotOK := nestedPortNumber(v)
		assert.Equal(t, ok, gotOK)
		if ok {
			assert.Equal(t, want, got)
		}
	}

	assertPort(int64(4317), 4317, true)
	assertPort(int(4317), 4317, true)
	assertPort(int32(4317), 4317, true)
	assertPort(float64(4317), 4317, true)
	assertPort(float64(4317.5), 0, false)
	assertPort(json.Number("4317"), 4317, true)
	assertPort("4317", 0, false)
}
