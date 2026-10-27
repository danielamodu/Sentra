package sim

import (
	"encoding/json"
	"fmt"
)

// mustJSON serializes v as canonical one-line JSON. Serializers for the
// recorded experiment inputs (workload spec, fault schedule) go through
// this; a marshal failure of plain data structures is a programming error.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("sim: marshal %T: %v", v, err))
	}
	return string(b)
}
