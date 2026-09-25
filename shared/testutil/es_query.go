package testutil

import (
	"encoding/json"
	"fmt"

	"github.com/olivere/elastic/v7"
)

// matches evaluates the query types the services use (bool, term, terms, range, exists,
// match_all) against a JSON document, so tests exercise the real filter logic. Query types
// it does not know (match, knn, …) match every document, as the mock did before.
func matches(q elastic.Query, doc map[string]interface{}) bool {
	if q == nil {
		return true
	}
	src, err := q.Source()
	if err != nil {
		return true
	}
	b, _ := json.Marshal(src)
	var m map[string]interface{}
	if json.Unmarshal(b, &m) != nil {
		return true
	}
	return eval(m, doc)
}

func eval(q map[string]interface{}, doc map[string]interface{}) bool {
	for kind, body := range q {
		switch kind {
		case "bool":
			return evalBool(body.(map[string]interface{}), doc)
		case "term":
			for field, v := range body.(map[string]interface{}) {
				if vm, ok := v.(map[string]interface{}); ok {
					v = vm["value"]
				}
				if !equal(doc[field], v) {
					return false
				}
			}
		case "terms":
			for field, vs := range body.(map[string]interface{}) {
				list, ok := vs.([]interface{})
				if !ok {
					continue
				}
				hit := false
				for _, v := range list {
					if equal(doc[field], v) {
						hit = true
						break
					}
				}
				if !hit {
					return false
				}
			}
		case "range":
			for field, spec := range body.(map[string]interface{}) {
				if !inRange(doc[field], spec.(map[string]interface{})) {
					return false
				}
			}
		case "exists":
			f, _ := body.(map[string]interface{})["field"].(string)
			if _, ok := doc[f]; !ok {
				return false
			}
		}
	}
	return true
}

func clauses(v interface{}) []map[string]interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{t}
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(t))
		for _, c := range t {
			if m, ok := c.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func evalBool(b map[string]interface{}, doc map[string]interface{}) bool {
	for _, key := range []string{"must", "filter"} {
		for _, c := range clauses(b[key]) {
			if !eval(c, doc) {
				return false
			}
		}
	}
	for _, c := range clauses(b["must_not"]) {
		if eval(c, doc) {
			return false
		}
	}
	should := clauses(b["should"])
	if len(should) > 0 {
		hit := false
		for _, c := range should {
			if eval(c, doc) {
				hit = true
				break
			}
		}
		// With must/filter present ES treats should as optional unless minimum_should_match
		// is set; the services always set it or use should alone.
		_, hasMin := b["minimum_should_match"]
		if !hit && (hasMin || (b["must"] == nil && b["filter"] == nil)) {
			return false
		}
	}
	return true
}

func equal(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func inRange(v interface{}, spec map[string]interface{}) bool {
	if v == nil {
		return false // like ES: a missing field never matches a range
	}
	cmp := func(bound interface{}) int {
		if fa, ok := v.(float64); ok {
			if fb, ok := bound.(float64); ok {
				switch {
				case fa < fb:
					return -1
				case fa > fb:
					return 1
				}
				return 0
			}
		}
		sa, sb := fmt.Sprint(v), fmt.Sprint(bound)
		switch {
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		}
		return 0
	}
	// olivere/elastic encodes ranges as from/to + include_lower/include_upper.
	incLower, incUpper := true, true
	if x, ok := spec["include_lower"].(bool); ok {
		incLower = x
	}
	if x, ok := spec["include_upper"].(bool); ok {
		incUpper = x
	}
	if from, ok := spec["from"]; ok && from != nil {
		c := cmp(from)
		if c < 0 || (c == 0 && !incLower) {
			return false
		}
	}
	if to, ok := spec["to"]; ok && to != nil {
		c := cmp(to)
		if c > 0 || (c == 0 && !incUpper) {
			return false
		}
	}
	for op, bound := range spec {
		c := cmp(bound)
		switch op {
		case "gt":
			if c <= 0 {
				return false
			}
		case "gte":
			if c < 0 {
				return false
			}
		case "lt":
			if c >= 0 {
				return false
			}
		case "lte":
			if c > 0 {
				return false
			}
		}
	}
	return true
}
