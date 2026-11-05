package mock

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
)

// errInvalidRequest marks a request error that the client caused.
var errInvalidRequest = errors.New("invalid request")

// pickTool returns the tool to call, or nil for a text-only reply. It follows
// tool_choice; for "auto" the seed decides. Server tools have no input
// schema and are never picked.
func pickTool(r *rand.Rand, req *request) (*tool, error) {
	var tools []*tool
	for i := range req.Tools {
		if req.Tools[i].Name != "" && len(req.Tools[i].InputSchema) > 0 {
			tools = append(tools, &req.Tools[i])
		}
	}
	choice := "auto"
	if req.ToolChoice != nil {
		choice = req.ToolChoice.Type
	}
	switch choice {
	case "none":
		return nil, nil
	case "tool":
		for _, t := range tools {
			if t.Name == req.ToolChoice.Name {
				return t, nil
			}
		}
		return nil, fmt.Errorf("%w: tool_choice names tool %q, which tools does not declare", errInvalidRequest, req.ToolChoice.Name)
	case "any":
		if len(tools) == 0 {
			return nil, fmt.Errorf("%w: tool_choice any needs at least one tool", errInvalidRequest)
		}
		return tools[r.IntN(len(tools))], nil
	case "auto":
		if len(tools) == 0 || r.IntN(2) == 0 {
			return nil, nil
		}
		return tools[r.IntN(len(tools))], nil
	default:
		return nil, fmt.Errorf("%w: unknown tool_choice type %q", errInvalidRequest, choice)
	}
}

// schema is the JSON Schema subset that toolArgs fills.
type schema struct {
	Type       any                `json:"type"`
	Properties map[string]*schema `json:"properties"`
	Items      *schema            `json:"items"`
	Enum       []any              `json:"enum"`
}

const maxSchemaDepth = 8

// toolArgs returns lorem arguments that fit the input schema.
func toolArgs(r *rand.Rand, raw json.RawMessage) ([]byte, error) {
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%w: tool input_schema: %w", errInvalidRequest, err)
	}
	v := genValue(r, &s, 0)
	if _, ok := v.(map[string]any); !ok {
		v = map[string]any{}
	}
	return json.Marshal(v)
}

func genValue(r *rand.Rand, s *schema, depth int) any {
	if s == nil || depth > maxSchemaDepth {
		return nil
	}
	if len(s.Enum) > 0 {
		return s.Enum[r.IntN(len(s.Enum))]
	}
	switch s.typeName() {
	case "object":
		obj := make(map[string]any, len(s.Properties))
		// Map order is random; sorted keys keep the seed's draws stable.
		for _, k := range slices.Sorted(maps.Keys(s.Properties)) {
			obj[k] = genValue(r, s.Properties[k], depth+1)
		}
		return obj
	case "array":
		items := s.Items
		if items == nil {
			items = &schema{Type: "string"}
		}
		arr := make([]any, 1+r.IntN(2))
		for i := range arr {
			arr[i] = genValue(r, items, depth+1)
		}
		return arr
	case "string":
		words := make([]string, 1+r.IntN(4))
		for i := range words {
			words[i] = loremWords[r.IntN(len(loremWords))]
		}
		return strings.Join(words, " ")
	case "integer":
		return r.IntN(100)
	case "number":
		return math.Round(r.Float64()*10000) / 100
	case "boolean":
		return r.IntN(2) == 1
	default:
		return nil
	}
}

// typeName returns the schema type. A type list gives its first non-null
// entry; a schema with properties and no type is an object.
func (s *schema) typeName() string {
	switch t := s.Type.(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if name, ok := v.(string); ok && name != "null" {
				return name
			}
		}
	}
	if s.Properties != nil {
		return "object"
	}
	return ""
}
