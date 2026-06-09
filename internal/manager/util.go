package manager

import "encoding/json"

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
