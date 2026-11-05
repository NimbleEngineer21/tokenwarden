package mock

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/NimbleEngineer21/tokenwarden/internal/anthropic"
)

// presetTools loads the fake MCP preset tools and returns them as a "tools"
// field and as a map from name to schema.
func presetTools(t *testing.T) (string, map[string]*schema) {
	t.Helper()
	raw, err := os.ReadFile("testdata/mcp_tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools []tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]*schema{}
	for _, tl := range tools {
		var s schema
		if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
			t.Fatal(err)
		}
		schemas[tl.Name] = &s
	}
	return `,"tools":` + string(raw), schemas
}

// checkValue fails unless v fits s.
func checkValue(t *testing.T, s *schema, v any, path string) {
	t.Helper()
	if len(s.Enum) > 0 {
		if !slices.Contains(s.Enum, v) {
			t.Fatalf("%s = %v, not in enum %v", path, v, s.Enum)
		}
		return
	}
	ok := true
	switch s.typeName() {
	case "object":
		m, isMap := v.(map[string]any)
		ok = isMap
		for k, ps := range s.Properties {
			pv, present := m[k]
			if !present {
				t.Fatalf("%s.%s is missing", path, k)
			}
			checkValue(t, ps, pv, path+"."+k)
		}
	case "array":
		arr, isArr := v.([]any)
		ok = isArr && len(arr) > 0
		for i, item := range arr {
			checkValue(t, s.Items, item, fmt.Sprintf("%s[%d]", path, i))
		}
	case "string":
		_, ok = v.(string)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && f == math.Trunc(f)
	case "number":
		_, ok = v.(float64)
	case "boolean":
		_, ok = v.(bool)
	}
	if !ok {
		t.Fatalf("%s = %#v does not fit type %q", path, v, s.typeName())
	}
}

func toolReply(t *testing.T, s *Server, prompt, extra string) (streamResult, *anthropic.ContentBlock) {
	t.Helper()
	res := parseStream(t, serve(t, s, msgBody(prompt, 1000, true, extra)).body())
	for _, ev := range res.events {
		if ev.Type == anthropic.EventContentBlockStart && ev.ContentBlock.Type == anthropic.BlockToolUse {
			if ev.Index != 1 {
				t.Fatalf("tool_use block at index %d, want 1", ev.Index)
			}
			return res, ev.ContentBlock
		}
	}
	return res, nil
}

func TestToolChoiceTool(t *testing.T) {
	tools, schemas := presetTools(t)
	s := New(Config{})
	for name, sc := range schemas {
		t.Run(name, func(t *testing.T) {
			res, blk := toolReply(t, s, "use "+name, tools+fmt.Sprintf(`,"tool_choice":{"type":"tool","name":%q}`, name))
			if blk == nil || blk.Name != name || !strings.HasPrefix(blk.ID, "toolu_mock_") {
				t.Fatalf("tool block = %+v", blk)
			}
			if res.stop != stopToolUse {
				t.Fatalf("stop_reason = %q", res.stop)
			}
			var input any
			if err := json.Unmarshal([]byte(res.text[1]), &input); err != nil {
				t.Fatalf("partial_json does not join into JSON: %v", err)
			}
			checkValue(t, sc, input, "input")

			// Each partial_json chunk counts as one token.
			chunks := 0
			for _, ev := range res.events {
				if ev.Type == anthropic.EventContentBlockDelta && ev.Index == 1 {
					chunks++
				}
			}
			want := int64(len(strings.Fields(res.text[0])) + chunks)
			if res.usage.OutputTokens != want {
				t.Fatalf("output_tokens = %d, want %d", res.usage.OutputTokens, want)
			}
		})
	}
}

func TestToolChoiceModes(t *testing.T) {
	tools, schemas := presetTools(t)
	s := New(Config{})
	used, skipped := 0, 0
	for i := range 40 {
		prompt := fmt.Sprintf("request %d", i)
		_, blk := toolReply(t, s, prompt, tools+`,"tool_choice":{"type":"any"}`)
		if blk == nil || schemas[blk.Name] == nil {
			t.Fatalf("any: tool block = %+v", blk)
		}
		if _, blk = toolReply(t, s, prompt, tools+`,"tool_choice":{"type":"none"}`); blk != nil {
			t.Fatalf("none: got tool %s", blk.Name)
		}
		if _, blk = toolReply(t, s, prompt, tools); blk != nil {
			used++
		} else {
			skipped++
		}
	}
	if used == 0 || skipped == 0 {
		t.Fatalf("auto: %d tool replies, %d text replies; want both", used, skipped)
	}
}

func TestToolErrors(t *testing.T) {
	tools, _ := presetTools(t)
	s := New(Config{})
	for _, extra := range []string{
		tools + `,"tool_choice":{"type":"tool","name":"mcp__slack__delete_channel"}`,
		`,"tool_choice":{"type":"any"}`,
		`,"tools":[{"name":"bad","input_schema":"not a schema"}],"tool_choice":{"type":"any"}`,
	} {
		if rec := serve(t, s, msgBody("x", 100, true, extra)); rec.code != 400 {
			t.Errorf("%s: status %d, want 400", extra, rec.code)
		}
	}
}

func TestToolSkippedWhenOverMaxTokens(t *testing.T) {
	tools, _ := presetTools(t)
	extra := tools + `,"tool_choice":{"type":"tool","name":"mcp__github__create_issue"}`
	res := parseStream(t, serve(t, New(Config{}), msgBody("tight", 8, true, extra)).body())
	if _, ok := res.text[1]; ok || res.stop != stopMaxTokens {
		t.Fatalf("stop = %q, blocks = %d; want max_tokens and no tool block", res.stop, len(res.text))
	}
}

func TestNonStreamToolInput(t *testing.T) {
	tools, _ := presetTools(t)
	extra := tools + `,"tool_choice":{"type":"tool","name":"mcp__filesystem__write_file"}`
	s := New(Config{})
	rec := serve(t, s, msgBody("write", 1000, false, extra))
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(rec.body()), &m); err != nil {
		t.Fatal(err)
	}
	res, _ := toolReply(t, s, "write", extra)
	if len(m.Content) != 2 || m.Content[1].Name != "mcp__filesystem__write_file" || string(m.Content[1].Input) != res.text[1] {
		t.Fatalf("non-stream content = %+v, stream input %s", m.Content, res.text[1])
	}
}

func TestGenValue(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	tests := []struct {
		schema string
		check  func(any) bool
	}{
		{`{"type":["null","integer"]}`, func(v any) bool { _, ok := v.(int); return ok }},
		{`{"properties":{"a":{"type":"boolean"}}}`, func(v any) bool { _, ok := v.(map[string]any)["a"].(bool); return ok }},
		{`{"type":"array"}`, func(v any) bool { a, ok := v.([]any); return ok && len(a) > 0 }},
		{`{"enum":[7]}`, func(v any) bool { return v == float64(7) }},
		{`{"type":"mystery"}`, func(v any) bool { return v == nil }},
	}
	for _, tt := range tests {
		var s schema
		if err := json.Unmarshal([]byte(tt.schema), &s); err != nil {
			t.Fatal(err)
		}
		if v := genValue(r, &s, 0); !tt.check(v) {
			t.Errorf("%s gave %#v", tt.schema, v)
		}
	}

	deep := `{"type":"string"}`
	for range maxSchemaDepth + 4 {
		deep = `{"type":"object","properties":{"n":` + deep + `}}`
	}
	b, err := toolArgs(r, json.RawMessage(deep))
	if err != nil || !strings.Contains(string(b), "null") {
		t.Fatalf("deep schema: %s, %v; want a null below the depth limit", b, err)
	}
}
