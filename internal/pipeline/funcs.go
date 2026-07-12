package pipeline

import (
	"fmt"
	"strconv"
	"strings"
)

// The Fabric pipeline function library (a faithful subset). Each entry takes
// the evaluated arguments and the context; unknown functions error out so a
// bad definition fails loudly rather than mis-running.
func callFunc(name string, args []value, ctx *evalContext) (value, error) {
	switch name {
	// --- system objects ---
	case "pipeline":
		return map[string]value{"parameters": asMap(ctx.Parameters), "globalParameters": map[string]value{}}, nil
	case "variables":
		if err := arity(name, args, 1); err != nil {
			return nil, err
		}
		v, ok := ctx.Variables[toString(args[0])]
		if !ok {
			return nil, fmt.Errorf("no such variable %q", toString(args[0]))
		}
		return v, nil
	case "activity":
		if err := arity(name, args, 1); err != nil {
			return nil, err
		}
		a, ok := ctx.Activities[toString(args[0])]
		if !ok {
			return nil, fmt.Errorf("no output for activity %q (not yet run?)", toString(args[0]))
		}
		return a, nil
	case "item":
		if !ctx.HasItem {
			return nil, fmt.Errorf("item() is only valid inside a ForEach")
		}
		return ctx.Item, nil

	// --- strings ---
	case "concat":
		var b strings.Builder
		for _, a := range args {
			b.WriteString(toString(a))
		}
		return b.String(), nil
	case "toUpper":
		return strings.ToUpper(toString(one(args))), nil
	case "toLower":
		return strings.ToLower(toString(one(args))), nil
	case "trim":
		return strings.TrimSpace(toString(one(args))), nil
	case "length":
		return float64(length(one(args))), nil
	case "substring":
		s := toString(args[0])
		start, n := int(toNumber(args[1])), int(toNumber(args[2]))
		if start < 0 || start+n > len(s) {
			return nil, fmt.Errorf("substring out of range")
		}
		return s[start : start+n], nil
	case "replace":
		return strings.ReplaceAll(toString(args[0]), toString(args[1]), toString(args[2])), nil
	case "startsWith":
		return strings.HasPrefix(toString(args[0]), toString(args[1])), nil
	case "endsWith":
		return strings.HasSuffix(toString(args[0]), toString(args[1])), nil
	case "guid":
		return "00000000-0000-0000-0000-000000000000", nil

	// --- logical / comparison ---
	case "equals":
		return equal(args[0], args[1]), nil
	case "not":
		return !toBool(one(args)), nil
	case "and":
		for _, a := range args {
			if !toBool(a) {
				return false, nil
			}
		}
		return true, nil
	case "or":
		for _, a := range args {
			if toBool(a) {
				return true, nil
			}
		}
		return false, nil
	case "greater":
		return toNumber(args[0]) > toNumber(args[1]), nil
	case "greaterOrEquals":
		return toNumber(args[0]) >= toNumber(args[1]), nil
	case "less":
		return toNumber(args[0]) < toNumber(args[1]), nil
	case "lessOrEquals":
		return toNumber(args[0]) <= toNumber(args[1]), nil
	case "if":
		if toBool(args[0]) {
			return args[1], nil
		}
		return args[2], nil
	case "coalesce":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "empty":
		return length(one(args)) == 0, nil
	case "contains":
		return contains(args[0], args[1]), nil

	// --- math ---
	case "add":
		return toNumber(args[0]) + toNumber(args[1]), nil
	case "sub":
		return toNumber(args[0]) - toNumber(args[1]), nil
	case "mul":
		return toNumber(args[0]) * toNumber(args[1]), nil
	case "div":
		d := toNumber(args[1])
		if d == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return toNumber(args[0]) / d, nil
	case "mod":
		return float64(int(toNumber(args[0])) % int(toNumber(args[1]))), nil
	case "max":
		return reduce(args, func(a, b float64) float64 { return maxf(a, b) }), nil
	case "min":
		return reduce(args, func(a, b float64) float64 { return minf(a, b) }), nil

	// --- conversions / arrays ---
	case "int":
		return float64(int(toNumber(one(args)))), nil
	case "float":
		return toNumber(one(args)), nil
	case "string":
		return toString(one(args)), nil
	case "bool":
		return toBool(one(args)), nil
	case "createArray":
		return append([]value{}, args...), nil
	case "range":
		start, count := int(toNumber(args[0])), int(toNumber(args[1]))
		out := make([]value, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, float64(start+i))
		}
		return out, nil
	case "first":
		arr := toArray(one(args))
		if len(arr) == 0 {
			return nil, nil
		}
		return arr[0], nil
	case "last":
		arr := toArray(one(args))
		if len(arr) == 0 {
			return nil, nil
		}
		return arr[len(arr)-1], nil
	}
	return nil, fmt.Errorf("unsupported function %q", name)
}

func arity(name string, args []value, n int) error {
	if len(args) != n {
		return fmt.Errorf("%s expects %d argument(s), got %d", name, n, len(args))
	}
	return nil
}

func one(args []value) value {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

func asMap(m map[string]value) map[string]value {
	if m == nil {
		return map[string]value{}
	}
	return m
}

func reduce(args []value, f func(a, b float64) float64) float64 {
	acc := toNumber(args[0])
	for _, a := range args[1:] {
		acc = f(acc, toNumber(a))
	}
	return acc
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// --- value coercions (ADF-style loose typing) --------------------------------

func toString(v value) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func toNumber(v value) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return n
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func toBool(v value) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	case float64:
		return t != 0
	}
	return false
}

func toArray(v value) []value {
	if t, ok := v.([]value); ok {
		return t
	}
	return nil
}

func length(v value) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []value:
		return len(t)
	case map[string]value:
		return len(t)
	case nil:
		return 0
	}
	return 0
}

func equal(a, b value) bool {
	// Numbers compare numerically; everything else by string form (ADF is loose).
	_, an := a.(float64)
	_, bn := b.(float64)
	if an && bn {
		return toNumber(a) == toNumber(b)
	}
	if ab, ok := a.(bool); ok {
		if bb, ok2 := b.(bool); ok2 {
			return ab == bb
		}
	}
	return toString(a) == toString(b)
}

func contains(coll, item value) bool {
	if s, ok := coll.(string); ok {
		return strings.Contains(s, toString(item))
	}
	for _, e := range toArray(coll) {
		if equal(e, item) {
			return true
		}
	}
	if m, ok := coll.(map[string]value); ok {
		_, ok := m[toString(item)]
		return ok
	}
	return false
}
