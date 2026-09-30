package hookrun

import (
	"encoding/json"
)

func quoteJSON(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}
