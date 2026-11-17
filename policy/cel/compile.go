package cel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
)

// costLimit stops an expression that runs too long, such as a large
// comprehension. Normal rules on a request cost far less.
const costLimit = 100_000

// request is the CEL view of a [policy.Input]. The cel tags give the field
// names that expressions use.
type request struct {
	Team      string   `cel:"team"`
	User      string   `cel:"user"`
	Model     string   `cel:"model"`
	MaxTokens int64    `cel:"max_tokens"`
	Tools     []string `cel:"tools"`
}

func newEnv() (*celgo.Env, error) {
	t := reflect.TypeFor[request]()
	return celgo.NewEnv(
		ext.NativeTypes(t, ext.ParseStructTags(true)),
		celgo.Variable("request", celgo.ObjectType("cel."+t.Name())),
		celgo.Variable("grants", celgo.ListType(celgo.StringType)),
	)
}

type rule struct {
	name   string
	match  matchSpec
	effect Effect
	reason string
	prg    celgo.Program
}

// ruleSet is one loaded version of the policy file. It does not change
// after compile returns, so many goroutines can read it.
type ruleSet struct {
	mode     Mode
	rules    []rule
	revision string
}

// compile parses data and compiles every expression in it.
func compile(env *celgo.Env, data []byte) (*ruleSet, error) {
	f, err := parseFile(data)
	if err != nil {
		return nil, err
	}
	rs := &ruleSet{
		mode:     f.Mode,
		rules:    make([]rule, 0, len(f.Rules)),
		revision: revisionOf(data),
	}
	for i, r := range f.Rules {
		prg, err := compileExpr(env, r.Expr)
		if err != nil {
			return nil, fmt.Errorf("rule %d %q: %w", i+1, r.Name, err)
		}
		rs.rules = append(rs.rules, rule{
			name:   r.Name,
			match:  r.Match,
			effect: r.Effect,
			reason: r.Reason,
			prg:    prg,
		})
	}
	return rs, nil
}

func compileExpr(env *celgo.Env, expr string) (celgo.Program, error) {
	if expr == "" {
		expr = "true"
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != celgo.BoolType {
		return nil, fmt.Errorf("expr has type %s, not bool", ast.OutputType())
	}
	return env.Program(ast, celgo.CostLimit(costLimit))
}

func revisionOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
